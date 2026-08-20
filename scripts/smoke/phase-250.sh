#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 250 smoke — same-runtime organization skill publications (HA-68).
# The integrated checkpoint has the domain, persistence, canonical wire
# contract, strict handler, clients/capability, generated pages, run-start
# composition, and shared production/devstack bootstrap. These guards pin the
# evidence without implying broad preflight.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-250-same-runtime-skill-publications.md" \
    "phase 250 plan exists"
assert_file \
    "internal/skills/publication/publication.go" \
    "phase 250 publication domain exists"
assert_file \
    "internal/protocol/types/skill_publications.go" \
    "phase 250 canonical wire types exist"
assert_file \
    "internal/protocol/skill_publications.go" \
    "phase 250 authorized Protocol surface exists"
assert_file \
    "internal/protocol/transports/control/skill_publications_handler.go" \
    "phase 250 strict control handler exists"
assert_file \
    "internal/protocol/client/client.go" \
    "phase 250 typed Protocol client exists"
assert_file \
    "internal/protocol/client/client_skill_publications_test.go" \
    "phase 250 typed Protocol client tests exist"
assert_file \
    "internal/runtime/serve/runloop.go" \
    "phase 250 run-start composition exists"
assert_file \
    "internal/runtime/serve/runloop_publication_test.go" \
    "phase 250 run-start composition tests exist"
assert_file \
    "internal/runtime/serve/publication_wiring.go" \
    "phase 250 shared publication wiring exists"
assert_file \
    "internal/runtime/serve/publication_wiring_test.go" \
    "phase 250 serve publication wiring tests exist"
assert_file \
    "harbortest/devstack/publication_wiring_test.go" \
    "phase 250 devstack publication wiring tests exist"

assert_grep_present \
    'type Store interface' \
    "internal/skills/publication/publication.go" \
    "phase 250 publication store contract exists"
assert_grep_present \
    'TestMemoryStore_PublicationLifecycleAndContentFreeProjections' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 lifecycle/content-free test exists"
assert_grep_present \
    'TestMemoryStore_RuntimeAndTenantIsolationFailClosed' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 identity/runtime isolation test exists"
assert_grep_present \
    'TestMemoryStore_ConcurrentResolveN128' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 concurrent resolve test exists"
assert_grep_present \
    'TestStateStoreStore_RestartAndCAS' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 StateStore CAS/restart test exists"
assert_grep_present \
    'TestStateStoreStore_ResponseLossAndUserScopedReferences' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 response-loss/reference test exists"

assert_grep_present \
    'NewSkillPublicationsSurface' \
    "internal/protocol/skill_publications.go" \
    "phase 250 authorized surface constructor exists"
assert_grep_present \
    'Store publication\.Store' \
    "internal/protocol/skill_publications.go" \
    "phase 250 surface requires an authorized publication store"
assert_grep_present \
    'requireAdmin' \
    "internal/protocol/skill_publications.go" \
    "phase 250 admin authority gate exists"
assert_grep_present \
    'requireReach' \
    "internal/protocol/skill_publications.go" \
    "phase 250 signed effective-agent reach gate exists"
assert_grep_present \
    'TestSkillPublicationsSurface_AdminPublishAndSignedAgentReachInstall' \
    "internal/protocol/skill_publications_test.go" \
    "phase 250 authorized store surface test exists"
assert_grep_present \
    'TestSkillPublicationsSurface_BodyIdentityCannotGrantAdminAuthority' \
    "internal/protocol/skill_publications_test.go" \
    "phase 250 body identity cannot grant authority test exists"

assert_grep_present \
    'serveSkillPublications' \
    "internal/protocol/transports/control/skill_publications_handler.go" \
    "phase 250 strict handler dispatch exists"
assert_grep_present \
    'decodeSkillPublicationsRequest' \
    "internal/protocol/transports/control/skill_publications_handler.go" \
    "phase 250 publication request decoder exists"
assert_grep_present \
    'decodeStrict' \
    "internal/protocol/transports/control/skill_publications_handler.go" \
    "phase 250 handler uses strict decoding"
assert_grep_present \
    'DisallowUnknownFields' \
    "internal/protocol/transports/control/control.go" \
    "phase 250 strict decoder rejects unknown fields"
assert_grep_present \
    'if methods\.IsSkillPublicationMethod\(method\)' \
    "internal/protocol/transports/control/control.go" \
    "phase 250 control mux routes publication methods"
assert_grep_present \
    'TestSkillPublicationsHandler_StrictDecodeAndDispatch' \
    "internal/protocol/transports/control/skill_publications_handler_test.go" \
    "phase 250 strict handler test exists"

assert_grep_present \
    'SkillPublicationsPublish' \
    "internal/protocol/client/client.go" \
    "phase 250 client publish method exists"
assert_grep_present \
    'SkillPublicationsReferencesList' \
    "internal/protocol/client/client.go" \
    "phase 250 client reference method exists"
assert_grep_present \
    'RuntimeClient = internal\.RuntimeClient' \
    "sdk/protocolclient/protocolclient.go" \
    "phase 250 public client alias exists"
assert_grep_present \
    'TestRuntimeClient_SkillPublicationsMethods_UseCanonicalRoutesAndClientIdentity' \
    "internal/protocol/client/client_skill_publications_test.go" \
    "phase 250 client route/identity test exists"
assert_grep_present \
    'TestRuntimeClient_SkillPublicationsMethodSet' \
    "internal/protocol/client/client_skill_publications_test.go" \
    "phase 250 client method-set test covers all ten routes"

assert_grep_present \
    'CapSkillPublications' \
    "internal/protocol/types/version.go" \
    "phase 250 canonical capability is registered"
assert_grep_present \
    'SkillPublicationsAvailable bool' \
    "internal/protocol/posture.go" \
    "phase 250 capability advertisement is conditional"
assert_grep_present \
    'SkillPublicationsAvailable' \
    "internal/protocol/posture.go" \
    "phase 250 posture wires the publication capability"

assert_grep_present \
    'PublicationStore' \
    "internal/runtime/serve/runloop.go" \
    "phase 250 run loop accepts the publication store"
assert_grep_present \
    'captureRunSkillSnapshot' \
    "internal/runtime/serve/runloop.go" \
    "phase 250 run-start snapshot capture exists"
assert_grep_present \
    'publicationStore\.Resolve' \
    "internal/runtime/serve/runloop.go" \
    "phase 250 composition resolves exact publication references"
assert_grep_present \
    'TestRunLoopDriver_PublicationSnapshotUsesOnePinnedReader' \
    "internal/runtime/serve/runloop_publication_test.go" \
    "phase 250 immutable snapshot test exists"
assert_grep_present \
    'TestRunLoopDriver_PublicationSnapshot_FailsClosedOnRetireAndAllowsMissingReference' \
    "internal/runtime/serve/runloop_publication_test.go" \
    "phase 250 run-start fail-closed test exists"
assert_grep_present \
    'TestRunLoopDriver_PublicationSnapshot_ConcurrentTupleIsolationN128' \
    "internal/runtime/serve/runloop_publication_test.go" \
    "phase 250 run-start N=128 tuple-isolation test exists"

assert_grep_present \
    'NewSkillPublicationStore' \
    "internal/runtime/serve/publication_wiring.go" \
    "phase 250 shared publication store constructor exists"
assert_grep_present \
    'publication\.NewStateStore' \
    "internal/runtime/serve/publication_wiring.go" \
    "phase 250 wiring uses the StateStore publication driver"
assert_grep_present \
    'publication\.NewAuthorizedStore' \
    "internal/runtime/serve/publication_wiring.go" \
    "phase 250 wiring wraps the store with signed reach"
assert_grep_present \
    'PublicationStore' \
    "internal/runtime/serve/mux.go" \
    "phase 250 serve mux accepts the shared publication store"
assert_grep_present \
    'PublicationRuntimeID' \
    "internal/runtime/serve/mux.go" \
    "phase 250 serve mux carries the immutable runtime id"
assert_grep_present \
    'WithSkillPublicationsSurface' \
    "internal/runtime/serve/mux.go" \
    "phase 250 serve mux mounts the Protocol surface"
assert_grep_present \
    'SkillPublicationsAvailable: publicationAvailable' \
    "internal/runtime/serve/mux.go" \
    "phase 250 serve mux gates capability on the mount"
assert_grep_present \
    'publication\.NewRuntimeID' \
    "internal/runtime/serve/serve.go" \
    "phase 250 production boot binds a runtime id"
assert_grep_present \
    'NewSkillPublicationStore' \
    "internal/runtime/serve/serve.go" \
    "phase 250 production boot mounts the shared store"
assert_grep_present \
    'PublicationStore:[[:space:]]+publicationStore' \
    "internal/runtime/serve/serve.go" \
    "phase 250 production boot passes the store to runtime and mux"
assert_grep_present \
    'NewSkillPublicationStore' \
    "harbortest/devstack/devstack.go" \
    "phase 250 devstack mounts the shared store"
assert_grep_present \
    'PublicationStore:[[:space:]]+publicationStore' \
    "harbortest/devstack/devstack.go" \
    "phase 250 devstack passes the store to runtime and mux"
assert_grep_present \
    'TestBuildMux_PublicationWiring_MountsCapabilityAndSharesState' \
    "internal/runtime/serve/publication_wiring_test.go" \
    "phase 250 serve mux shared-store test exists"
assert_grep_present \
    'TestBuildMux_PublicationWiring_ConcurrentReads' \
    "internal/runtime/serve/publication_wiring_test.go" \
    "phase 250 serve wiring N=128 test exists"
assert_grep_present \
    'TestBoot_PublicationsConfigured_MountsSharedStoreAndCapability' \
    "internal/runtime/serve/publication_wiring_test.go" \
    "phase 250 production boot wiring test exists"
assert_grep_present \
    'TestBoot_PublicationsWithoutAdmissionAuthority_UnmountedAndUnadvertised' \
    "internal/runtime/serve/publication_wiring_test.go" \
    "phase 250 production unavailable posture test exists"
assert_grep_present \
    'TestAssemble_PublicationWiring_ConfiguredKekMountsSharedStateAndCapability' \
    "harbortest/devstack/publication_wiring_test.go" \
    "phase 250 devstack wiring test exists"
assert_grep_present \
    'TestAssemble_PublicationWiring_WithoutAdmissionAuthorityStaysUnavailable' \
    "harbortest/devstack/publication_wiring_test.go" \
    "phase 250 devstack unavailable posture test exists"
assert_grep_present \
    'const n = 128' \
    "harbortest/devstack/publication_wiring_test.go" \
    "phase 250 devstack wiring N=128 test exists"

assert_grep_present \
    'skills.publications.publish' \
    "internal/protocol/methods/methods.go" \
    "phase 250 admin publish method is registered"
assert_grep_present \
    'skills.publications.references.list' \
    "internal/protocol/methods/methods.go" \
    "phase 250 caller reference method is registered"
assert_grep_present \
    'skill_publication_runtime_mismatch' \
    "internal/protocol/errors/errors.go" \
    "phase 250 runtime mismatch error is registered"
assert_grep_present \
    'SkillPublicationPublishRequest' \
    "docs/site/protocol/types.md" \
    "phase 250 generated types reference includes publication DTOs"
assert_grep_present \
    'skills\.publications\.publish' \
    "docs/site/protocol/methods.md" \
    "phase 250 generated methods reference includes publication routes"
assert_grep_present \
    'skill_publication_runtime_mismatch' \
    "docs/site/protocol/errors.md" \
    "phase 250 generated errors reference includes guidance"

assert_grep_present \
    'Organization skill publication' \
    "docs/skills/configure-memory-and-skills/SKILL.md" \
    "phase 250 operator skill documents publication lifecycle"
assert_grep_present \
    'same-runtime' \
    "docs/skills/use-the-harbor-protocol/SKILL.md" \
    "phase 250 Protocol skill documents runtime binding"
assert_grep_present \
    'organization skill publications' \
    "examples/harbor.yaml" \
    "phase 250 example config records publication posture"
assert_grep_present \
    'D-430' \
    "CHANGELOG.md" \
    "phase 250 changelog entry exists"
assert_grep_present \
    'HA-68 publication note' \
    "docs/site/protocol/index.md" \
    "phase 250 protocol-site mirror records shared bootstrap"

smoke_summary
