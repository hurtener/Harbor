#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 265 smoke — user-scoped signed OAuth MCP capability lifecycle.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-265-user-scoped-signed-oauth-mcp.md" "phase 265 plan exists"
assert_grep_present '^\|265 \| User-scoped signed OAuth MCP capability lifecycle' "docs/plans/README.md" "phase 265 canonical index row exists"
assert_grep_present '## D-448' "docs/decisions.md" "D-448 decision exists"
assert_grep_present 'AgentConfigUserRegisterOAuthMCPCapabilityRequest' "internal/protocol/types/agentconfig.go" "user register wire type exists"
assert_grep_present 'AgentConfigUserRemoveOAuthMCPCapabilityRequest' "internal/protocol/types/agentconfig.go" "user remove wire type exists"
assert_grep_present 'MethodAgentConfigUserRegisterOAuthMCPCapability' "internal/protocol/methods/methods.go" "user register method is canonical"
assert_grep_present 'MethodAgentConfigUserRemoveOAuthMCPCapability' "internal/protocol/methods/methods.go" "user remove method is canonical"
assert_grep_present 'user/register_oauth_mcp_capability' "internal/protocol/transports/stream/agentconfig_handler.go" "user register route exists"
assert_grep_present 'user/remove_oauth_mcp_capability' "internal/protocol/transports/stream/agentconfig_handler.go" "user remove route exists"
assert_grep_present 'ScopeAgentConfigUser' "internal/runtime/agentcfg/protocol/signed_oauth_scope.go" "user scope is required"
assert_grep_present 'AuthorizeAgentReach' "internal/runtime/agentcfg/protocol/signed_oauth_scope.go" "signed reach is required"
assert_grep_present 'ConfigScopeUser' "internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go" "registration uses user scope"
assert_grep_present 'ConfigScopeUser' "internal/runtime/agentcfg/protocol/remove_signed_oauth_mcp_capability.go" "removal uses user scope"
assert_grep_present 'UserID.*SessionID' "internal/agentcfg/signed_oauth_operation.go" "replay slot carries user/session"
assert_grep_present 'SubjectExactConnectionDetacher' "internal/runtime/agentcfg/protocol/addconnection.go" "subject exact detach seam exists"
assert_grep_present 'User string' "internal/tools/auth/owner.go" "physical owner carries optional user"
assert_grep_present 'OwnerOfSource' "internal/runtime/serve/mcp_detacher.go" "projection can resolve physical owner"
assert_grep_present 'userScopedMCPView' "internal/runtime/agentcfg/projection/projection.go" "acting-user projection narrowing exists"
assert_grep_present 'ServerLoadingModes map\[string\]string' "internal/protocol/types/agentconfig.go" "user loading server map is bounded"
assert_grep_present 'ToolLoadingModes map\[string\]string' "internal/protocol/types/agentconfig.go" "user loading tool map is bounded"
assert_grep_present 'validateUserToolExposureLoading' "internal/runtime/agentcfg/protocol/user.go" "user loading values are validated before write"
assert_grep_present 'composeLoadingView' "internal/runtime/agentcfg/projection/projection.go" "operator then user loading composition is shared"
assert_grep_present 'TestActivePlannerCatalogView_UserMCPServerLoadingModeUsesLogicalPairName' "internal/runtime/agentcfg/projection/user_mcp_projection_test.go" "logical user loading maps to physical source"
assert_grep_present 'TestExecutor_CallTool_GuessedNameCannotBypassRunCatalog' "internal/runtime/dispatch/dispatch_test.go" "ordinary dispatch cannot bypass run catalog"
assert_grep_present 'TestRegisterUserOAuthMCPCapability_ProductionPathAuthenticatesPerUserAndIsolatesCatalog' "internal/runtime/serve/signed_oauth_production_path_test.go" "authenticated discovery is per-user"
assert_grep_present 'OwnOAuthProvider: true' "internal/runtime/agentcfg/protocol/signed_oauth_mcp_reconcile.go" "pair provider remains private"
assert_grep_present 'TestUserSignedOAuthMCPCapability_TwoUsersHaveIndependentDesiredAndPhysicalOwners' "internal/runtime/agentcfg/protocol/user_signed_oauth_mcp_capability_test.go" "two-user lifecycle isolation is covered"
assert_grep_present 'TestActivePlannerCatalogView_UserMCPDesiredPairsAreIdentityScoped' "internal/runtime/agentcfg/projection/user_mcp_projection_test.go" "two-user projection isolation is covered"
assert_grep_present 'AgentConfigUserRegisterOAuthMCPCapabilityRequest' "docs/site/protocol/types.md" "generated type reference includes user register"
assert_grep_present 'agent_config.user.register_oauth_mcp_capability' "docs/site/protocol/methods.md" "generated method reference includes user register"

smoke_summary
