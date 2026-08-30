#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 267 smoke — same-runtime admin agent-pack inspect and copy.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The static contract intentionally pins the implementation seam and focused
# tests as well as the docs. Documentation alone cannot make this smoke green.
assert_file "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "phase 267 plan exists"
assert_file "internal/protocol/agent_packs.go" \
    "agent-pack Protocol surface exists"
assert_file "internal/protocol/agent_packs_test.go" \
    "agent-pack Protocol surface tests exist"
assert_file "internal/protocol/types/agentconfig.go" \
    "agent-pack wire types source exists"
assert_file "internal/protocol/singlesource/singlesource.go" \
    "canonical wire registration source exists"
assert_file "internal/protocol/client/client.go" \
    "typed Protocol client source exists"
assert_file "web/console/src/lib/protocol/agentconfig.ts" \
    "Console Protocol client source exists"
assert_file "web/console/src/lib/protocol/wire-manifest.gen.json" \
    "generated wire manifest exists"
assert_file "internal/protocol/transports/stream/agentconfig_agentpacks_handler_test.go" \
    "agent-pack typed route tests exist"
assert_file "internal/runtime/agentcfg/protocol/agentpacks_effective.go" \
    "agent-pack effective resolver/copy port exists"
assert_file "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "agent-pack effective resolver/copy tests exist"

assert_grep_present '^\|267 \| Same-runtime agent-pack inspect/copy' \
    "docs/plans/README.md" "phase 267 index exists"
assert_grep_present '^## D-456 ' "docs/decisions.md" "D-456 exists"
assert_grep_present '^\*\*Agent-pack semantic hash\*\*' \
    "docs/glossary.md" "agent-pack glossary terms exist"
assert_grep_present 'Same-runtime inspect and copy' \
    "docs/skills/use-the-harbor-protocol/SKILL.md" \
    "operator skill documents phase 267"
assert_grep_present 'Phase 267 candidate note' \
    "docs/site/protocol/index.md" "protocol site note exists"

assert_grep_present 'agent_config\.agent_packs\.inspect' \
    "internal/protocol/methods/methods.go" "inspect method is canonical"
assert_grep_present 'agent_config\.agent_packs\.copy' \
    "internal/protocol/methods/methods.go" "copy method is canonical"
assert_grep_present 'AgentConfigAgentPacksInspectRequest' \
    "internal/protocol/types/agentconfig.go" "inspect request is typed"
assert_grep_present 'AgentConfigAgentPacksInspectResponse' \
    "internal/protocol/types/agentconfig.go" "inspect response is typed"
assert_grep_present 'AgentConfigAgentPacksCopyRequest' \
    "internal/protocol/types/agentconfig.go" "copy request is typed"
assert_grep_present 'AgentConfigAgentPacksCopyResponse' \
    "internal/protocol/types/agentconfig.go" "copy response is typed"
assert_grep_present 'AgentConfigAgentPacksInspectRequest' \
    "internal/protocol/singlesource/singlesource.go" \
    "inspect request is in canonical wire registration"
assert_grep_present 'AgentConfigAgentPacksCopyRequest' \
    "internal/protocol/singlesource/singlesource.go" \
    "copy request is in canonical wire registration"
assert_grep_present 'agent_config\.agent_packs\.inspect' \
    "internal/protocol/transports/stream/agentconfig_handler.go" \
    "inspect route is mounted by the typed handler"
assert_grep_present 'agent_config\.agent_packs\.copy' \
    "internal/protocol/transports/stream/agentconfig_handler.go" \
    "copy route is mounted by the typed handler"
assert_grep_present 'AgentConfigAgentPacksInspect' \
    "internal/protocol/client/client.go" "inspect client route is typed"
assert_grep_present 'AgentConfigAgentPacksCopy' \
    "internal/protocol/client/client.go" "copy client route is typed"
assert_grep_present 'AgentConfigAgentPacksInspect' \
    "web/console/src/lib/protocol/agentconfig.ts" \
    "inspect Console type is mirrored"
assert_grep_present 'AgentConfigAgentPacksCopy' \
    "web/console/src/lib/protocol/agentconfig.ts" \
    "copy Console type is mirrored"
assert_grep_present 'boot_packs' "internal/protocol/types/agentconfig.go" \
    "boot layer is a distinct wire projection"
assert_grep_present 'revision_packs' "internal/protocol/types/agentconfig.go" \
    "revision layer is a distinct wire projection"
assert_grep_present 'effective_packs' "internal/protocol/types/agentconfig.go" \
    "effective layer is a wire projection"
assert_grep_present 'pack_ids' "internal/protocol/types/agentconfig.go" \
    "copy selection is bounded and plural"
assert_grep_present 'expected_source_composition_hash' \
    "internal/protocol/types/agentconfig.go" "source CAS is wire-required"
assert_grep_present 'expected_target_composition_hash' \
    "internal/protocol/types/agentconfig.go" "target CAS is wire-required"
assert_grep_present 'idempotency_key' "internal/protocol/types/agentconfig.go" \
    "copy idempotency is wire-required"
assert_grep_present 'CompositionHash' "internal/protocol/types/agentconfig.go" \
    "copy echoes the exact target composition hash"
assert_grep_present 'BootPackSetHash' "internal/protocol/types/agentconfig.go" \
    "copy echoes the exact target boot-set hash"
assert_grep_present 'func \(s \*Service\) InspectEffective' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective.go" \
    "runtime inspection port exists"
assert_grep_present 'func \(s \*Service\) CopySelected' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective.go" \
    "runtime copy port exists"
assert_grep_present 'OriginRef' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective.go" \
    "server-owned copy lineage is persisted"

assert_grep_present 'TestAgentPacksSurface_InspectAndCopy_EnforcesAdminIdentityAndReach' \
    "internal/protocol/agent_packs_test.go" "Protocol identity/reach gate is tested"
assert_grep_present 'TestAgentPacksSurface_CopyConflictIsClosedAndTyped' \
    "internal/protocol/agent_packs_test.go" "Protocol collision typing is tested"
assert_grep_present 'TestAgentPacksSurface_CopyAllowsEmptySelectionForReconciliation' \
    "internal/protocol/agent_packs_test.go" \
    "Protocol empty-selection reconciliation is tested"
assert_grep_present 'TestAgentPacksSurface_ConcurrentReuse_Isolated' \
    "internal/protocol/agent_packs_test.go" "Protocol concurrent reuse is tested"
assert_grep_present 'TestAgentConfigHandler_AgentPacks_UsesTypedRoutesAndGates' \
    "internal/protocol/transports/stream/agentconfig_agentpacks_handler_test.go" \
    "typed routes and gates are tested"
assert_grep_present 'TestAgentConfigHandler_AgentPacks_AbsentSurfaceFailsLoud' \
    "internal/protocol/transports/stream/agentconfig_agentpacks_handler_test.go" \
    "absent capability fails loudly"
assert_grep_present 'TestAgentPackInspectEffective_PreservesLayersAndDedupes' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "layer preservation and deduplication are tested"
assert_grep_present 'TestAgentPackCopySelected_CASIdempotencyReconciliationAndCollision' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "CAS/idempotency/reconciliation/collision are tested"
assert_grep_present 'TestAgentPackCopySelected_RejectsNilSelection' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "runtime nil-selection rejection is tested"
assert_grep_present 'TestAgentPackService_ConcurrentInspectReuse' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "runtime concurrent reuse is tested"
assert_grep_present 'TestAgentPackService_ConcurrentInspectCopyReuse' \
    "internal/runtime/agentcfg/protocol/agentpacks_effective_test.go" \
    "runtime inspect/copy concurrent reuse is tested"
assert_file "internal/runtime/serve/agentpacks_integration_test.go" \
    "real BuildMux agent-pack integration matrix exists"
assert_grep_present 'TestBuildMux_AgentPacks_RealStateStoreMatrix' \
    "internal/runtime/serve/agentpacks_integration_test.go" \
    "real StateStore driver matrix is tested"
assert_grep_present 'phase267-empty-reconcile-' \
    "internal/runtime/serve/agentpacks_integration_test.go" \
    "public HTTP empty-selection reconciliation is tested"

assert_grep_present 'one all-or-nothing target revision' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "copy atomicity is documented"
assert_grep_present 'same-runtime' "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "same-runtime boundary is documented"
assert_grep_present 'source/target composition' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "source and target CAS are documented"
assert_grep_present 'origin_ref' "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "copy lineage is documented"
assert_grep_present 'propose.*commit.*remove' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "existing target governance is documented"
assert_grep_present 'Protocol `0\.1\.0` is unchanged' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "Protocol version boundary is documented"
assert_grep_present '64 lowercase SHA-256 hex' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "hash encoding is canonical lowercase hex"
assert_grep_present 'deterministic empty' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "empty hash values are deterministic and present"
assert_grep_absent 'sha256:|all-zero|zero sentinel|000000000000' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "hash docs do not invent prefixes or empty sentinels"
assert_grep_present '^## \[1\.31\.0\]' "CHANGELOG.md" \
    "candidate changelog scope is documented"

# Avoid spelling the forbidden predecessor/product vocabulary in this source;
# the character-class form still makes the drift guard match case-insensitively.
assert_grep_absent '[Pp][Ee][Nn][Gg][Uu][Ii]' \
    "docs/plans/phase-267-agent-pack-inspect-copy.md" \
    "phase plan stays Harbor-only"

smoke_summary
