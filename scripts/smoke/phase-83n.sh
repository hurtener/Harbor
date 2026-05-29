#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83n — `harbor init` command + tiered yaml + AGENTS/CLAUDE/README
# templates + opt-in built-in tools (clock.now / text.echo) + docs/CONFIG.md
# reference + CI doc-drift gate. D-153.
#
# All assertions are static-only (file existence, grep). The cobra
# integration test lives in cmd/harbor/cmd_init_test.go; the engine
# test in cmd/harbor/init/init_test.go; the built-in mirror test in
# internal/tools/builtin/builtin_test.go; the doc-drift gate in
# internal/config/doc_drift_test.go.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Init engine + cobra wiring.
# ----------------------------------------------------------------------------
assert_file "cmd/harbor/cmd_init.go" \
    'harbor init cobra subcommand lives at the documented path'
assert_grep_present 'func newInitCmd' "cmd/harbor/cmd_init.go" \
    "newInitCmd constructor declared"
assert_grep_present 'CodeInitFileExists' "cmd/harbor/cmd_init.go" \
    "init_file_exists CLIError code declared"
assert_grep_present 'root\.AddCommand\(newInitCmd\(\)\)' "cmd/harbor/root.go" \
    "init subcommand registered on the cobra root"

assert_file "cmd/harbor/init/init.go" \
    "init engine package lives at cmd/harbor/init/"
assert_grep_present 'func Init' "cmd/harbor/init/init.go" \
    "harborinit.Init function declared"
assert_grep_present 'ErrFileExists' "cmd/harbor/init/init.go" \
    "ErrFileExists sentinel declared"

# ----------------------------------------------------------------------------
# Templates — four files, all under cmd/harbor/init/templates/default/.
# ----------------------------------------------------------------------------
assert_file "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "harbor.yaml template ships"
assert_file "cmd/harbor/init/templates/default/AGENTS.md.tmpl" \
    "AGENTS.md template ships"
assert_file "cmd/harbor/init/templates/default/CLAUDE.md.tmpl" \
    "CLAUDE.md template ships"
assert_file "cmd/harbor/init/templates/default/README.md.tmpl" \
    "README.md template ships"

# The yaml template references all four LLM-provider examples.
assert_grep_present 'Example 1: OpenRouter' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template carries the OpenRouter example block"
assert_grep_present 'Example 2: Anthropic' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template carries the Anthropic example block"
assert_grep_present 'Example 3: OpenAI' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template carries the OpenAI example block"
assert_grep_present 'Example 4: NVIDIA NIM' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template carries the NIM example block"

# Tiered structure is named in the comments.
assert_grep_present 'REQUIRED' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template flags the REQUIRED tier"
assert_grep_present 'COMMON KNOBS' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template flags the COMMON KNOBS tier"
assert_grep_present 'ADVANCED' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template flags the ADVANCED tier"

# The yaml template surfaces the opt-in built-in tools.
assert_grep_present 'built_in:' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template includes the tools.built_in opt-in surface"
assert_grep_present 'clock.now' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template names clock.now as a built-in"
assert_grep_present 'text.echo' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "yaml template names text.echo as a built-in"

# ----------------------------------------------------------------------------
# Built-in tools package.
# ----------------------------------------------------------------------------
assert_file "internal/tools/builtin/builtin.go" \
    "builtin package lives at internal/tools/builtin/"
assert_file "internal/tools/builtin/clock.go" \
    "clock.now lives at internal/tools/builtin/clock.go"
assert_file "internal/tools/builtin/text.go" \
    "text.echo lives at internal/tools/builtin/text.go"
assert_grep_present 'func Register' "internal/tools/builtin/builtin.go" \
    "builtin.Register dispatcher declared"
assert_grep_present 'ErrUnknownBuiltIn' "internal/tools/builtin/builtin.go" \
    "ErrUnknownBuiltIn sentinel declared"
assert_grep_present 'func KnownNames' "internal/tools/builtin/builtin.go" \
    "builtin.KnownNames() registry-mirror accessor declared"

# Mirror in the config validator.
assert_grep_present 'allowedBuiltInTools' "internal/config/validate.go" \
    "config validator carries the built-in allowlist mirror (§4.4 pattern)"
assert_grep_present 'KnownBuiltInTools' "internal/config/validate.go" \
    "config exposes KnownBuiltInTools() so the mirror test can compare"

# ----------------------------------------------------------------------------
# Wiring into dev binary + devstack mirror (D-094).
# ----------------------------------------------------------------------------
assert_grep_present 'builtin\.Register(With)?\(' \
    "cmd/harbor/cmd_dev.go" \
    "bootDevStack invokes builtin.Register / RegisterWith"
assert_grep_present 'cfg\.Tools\.BuiltIn' \
    "cmd/harbor/cmd_dev.go" \
    "bootDevStack passes cfg.Tools.BuiltIn into the registrar"
# Phase 107c (D-167) widened the registrar shape from `Register(cat, names)` to
# `RegisterWith(RegistryContext{Catalog, SkillStore, ArtifactStore}, names)`
# so the new meta-tools (skill_search / skill_get / artifact_fetch) can reach
# their backing stores. The legacy `Register` wrapper survives but the
# devstack mirror routes through `RegisterWith` to expose the dep slots.
assert_grep_present 'builtin\.Register(With)?\(' \
    "harbortest/devstack/devstack.go" \
    "devstack mirrors built-in registration (D-094)"
assert_grep_present 'cfg\.Tools\.BuiltIn' \
    "harbortest/devstack/devstack.go" \
    "devstack passes cfg.Tools.BuiltIn into the registrar (D-094)"

# ----------------------------------------------------------------------------
# docs/CONFIG.md + drift test.
# ----------------------------------------------------------------------------
assert_file "docs/CONFIG.md" \
    "configuration reference ships"
assert_grep_present '## Server' "docs/CONFIG.md" \
    "CONFIG.md has the Server section"
assert_grep_present '## Identity' "docs/CONFIG.md" \
    "CONFIG.md has the Identity section"
assert_grep_present '## LLM' "docs/CONFIG.md" \
    "CONFIG.md has the LLM section"
assert_grep_present '## Tools' "docs/CONFIG.md" \
    "CONFIG.md has the Tools section"
assert_grep_present '## Planner' "docs/CONFIG.md" \
    "CONFIG.md has the Planner section"
assert_grep_present '### tools.built_in' "docs/CONFIG.md" \
    "CONFIG.md documents the new tools.built_in field"

assert_file "internal/config/doc_drift_test.go" \
    "CONFIG.md drift gate ships"
assert_grep_present 'TestConfigDoc_AllFieldsDocumented' \
    "internal/config/doc_drift_test.go" \
    "drift gate test declared"

smoke_summary
