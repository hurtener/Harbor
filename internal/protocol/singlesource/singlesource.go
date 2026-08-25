// Package singlesource is the Harbor Protocol single-source enforcement
// checker (CLAUDE.md §8, §13; RFC §5). It is the formalisation
// of the single-source discipline laid the foundation
// for: the canonical Protocol packages are the ONLY definition sites,
// and a hardcoded Protocol method string / error code / wire-type
// redefinition anywhere else is a build-gating lint failure.
//
// # What is single-sourced, and where
//
//   - Protocol method names — internal/protocol/methods. Every method
//     wire string is a constant there; no method string literal appears
//     anywhere else under internal/protocol/ (CLAUDE.md §8: "No
//     hardcoded method strings elsewhere").
//   - Protocol error codes — internal/protocol/errors. Every Code
//     constant is declared there; no other package declares a
//     protocol/errors.Code constant (CLAUDE.md §8: "Add new codes there
//     and only there").
//   - Protocol wire types — internal/protocol/types. Every canonical
//     Protocol message struct is declared there; no other package
//     re-declares one (CLAUDE.md §13: "Adding a third place to define
//     Protocol message types" is rejection-on-sight).
//
// # Why a go/parser checker, not a golangci-lint analyzer or a script
//
// The repo already proves the pattern: internal/planner/conformance/
// importgraph_test.go is a go/parser AST walk that gates the §13
// planner-does-not-import-runtime invariant with zero external-tool
// dependencies. Harbor reuses that shape — a custom golangci-lint
// analyzer would need a plugin build and a .golangci.yml entry (a new
// linter needs a PR rationale per CLAUDE.md §5), and a shell script
// could not parse Go reliably (a method string inside a comment or a
// struct-tag is not a violation; only a real string-literal expression
// is). go/parser sees the AST, so the checker is precise: it flags a
// BasicLit STRING whose unquoted value is a canonical method name, not
// a substring match. The checker is plain Go, runs as a `go test`, and
// is gated by CI + the preflight smoke exactly like the importgraph
// lint.
//
// # The checker is a reusable artifact
//
// ScanProtocolTree and its helpers are pure functions over a filesystem
// root — no package-level mutable state, safe to call concurrently. The
// test is the first consumer; a later phase (e.g. a `harbor
// lint` subcommand, or the versioning discipline) can call the
// same checker without a second implementation.
package singlesource

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Violation is a single single-source breach found by a scan. It names
// the offending file (repo-relative), the 1-based line, the kind of
// breach, and a human-readable detail. A scan returns every Violation it
// finds in one pass so an operator sees the full extent of the drift,
// not just the first breach.
type Violation struct {
	// File is the offending file, relative to the scanned root.
	File string
	// Line is the 1-based line number of the offending token.
	Line int
	// Kind classifies the breach — one of the Kind* constants.
	Kind string
	// Detail is a human-readable explanation naming the offending
	// identifier / literal and the canonical package it belongs in.
	Detail string
}

// String renders a Violation as a single `file:line: kind: detail`
// line, the shape a test failure message joins.
func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Kind, v.Detail)
}

// The Kind* constants classify a Violation. They are stable strings so a
// test (or a future `harbor lint` subcommand) can branch on the kind.
const (
	// KindMethodLiteral — a Protocol method wire string appears as a
	// string literal outside internal/protocol/methods.
	KindMethodLiteral = "method-literal"
	// KindErrorCode — a protocol/errors.Code constant is declared
	// outside internal/protocol/errors.
	KindErrorCode = "error-code"
	// KindWireType — a canonical Protocol message struct type is
	// declared outside internal/protocol/types.
	KindWireType = "wire-type"
)

// CanonicalMethods is the set of Protocol method wire strings that must
// only ever appear as constants in internal/protocol/methods. It is
// kept in lockstep with internal/protocol/methods by
// TestSingleSource_CanonicalMethodsInLockstep — the checker does NOT
// import the methods package (the checker must be runnable against a
// tree where methods/ itself is the thing under audit), so the set is
// duplicated here and the test pins the duplication.
var CanonicalMethods = map[string]struct{}{
	"start":                           {},
	"cancel":                          {},
	"pause":                           {},
	"resume":                          {},
	"redirect":                        {},
	"inject_context":                  {},
	"approve":                         {},
	"reject":                          {},
	"prioritize":                      {},
	"user_message":                    {},
	"events.subscribe":                {},
	"events.aggregate":                {},
	"search.query":                    {},
	"search.sessions":                 {},
	"search.tasks":                    {},
	"search.events":                   {},
	"search.artifacts":                {},
	"runtime.info":                    {},
	"runtime.health":                  {},
	"runtime.counters":                {},
	"runtime.drivers":                 {},
	"metrics.snapshot":                {},
	"governance.posture":              {},
	"llm.posture":                     {},
	"governance.set_tenant_overrides": {},
	"governance.get_tenant_overrides": {},
	"governance.rotate_key":           {},
	"governance.set_posture":          {},
	// agent-config control-plane cluster — fourteen methods.
	"agent_config.get":                           {},
	"agent_config.set_revision":                  {},
	"agent_config.list_revisions":                {},
	"agent_config.diff":                          {},
	"agent_config.rollback":                      {},
	"agent_config.retire":                        {},
	"agent_config.skills.list":                   {},
	"agent_config.skills.upsert":                 {},
	"agent_config.skills.delete":                 {},
	"agent_config.agent_packs.list":              {},
	"agent_config.agent_packs.upsert":            {},
	"agent_config.agent_packs.remove":            {},
	"agent_config.agent_packs.propose":           {},
	"agent_config.agent_packs.commit":            {},
	"agent_config.set_tool_exposure":             {},
	"agent_config.set_prompt_layers":             {},
	"agent_config.set_extra_system_blocks":       {},
	"agent_config.set_llm_params":                {},
	"agent_config.add_mcp_connection":            {},
	"agent_config.remove_mcp_connection":         {},
	"agent_config.set_mcp_discovery_origins":     {},
	"agent_config.set_oauth_provider":            {},
	"agent_config.register_oauth_mcp_capability": {},
	"agent_config.remove_oauth_mcp_capability":   {},
	"agent_config.remove_oauth_provider":         {},
	"agent_config.set_llm_provider":              {},
	// HA-68 same-runtime organization skill-publication contract.
	"skills.publications.publish":           {},
	"skills.publications.list":              {},
	"skills.publications.get":               {},
	"skills.publications.publish_successor": {},
	"skills.publications.retire":            {},
	"skills.publications.available":         {},
	"skills.publications.install":           {},
	"skills.publications.update":            {},
	"skills.publications.remove":            {},
	"skills.publications.references.list":   {},
	// Session-user safe subset (the non-admin lower tier).
	"agent_config.session.set_user_prompt":     {},
	"agent_config.session.set_source_disables": {},
	"agent_config.session.skills.list":         {},
	"agent_config.session.skills.upsert":       {},
	"agent_config.session.skills.delete":       {},
	// User tier (the durable per-user config variant — the middle tier).
	"agent_config.user.get":                    {},
	"agent_config.user.set_revision":           {},
	"agent_config.user.list_revisions":         {},
	"agent_config.user.diff":                   {},
	"agent_config.user.rollback":               {},
	"agent_config.user.skills.list":            {},
	"agent_config.user.skills.upsert":          {},
	"agent_config.user.skills.delete":          {},
	"agent_config.user.skills.import_validate": {},
	"agent_config.user.skills.import_commit":   {},
	"agent_config.composition.preview":         {},
	"pause.list":                               {},
	"topology.snapshot":                        {},
	"artifacts.list":                           {},
	"artifacts.put":                            {},
	"artifacts.get":                            {},
	"artifacts.get_ref":                        {},
	"artifacts.delete":                         {},
	"memory.list":                              {},
	"memory.get":                               {},
	"memory.health":                            {},
	"memory.strategy_trace":                    {},
	"memory.put":                               {},
	"memory.delete":                            {},
	// MCP-Connections-page cluster — twelve methods.
	"mcp.servers.list":               {},
	"mcp.servers.get":                {},
	"mcp.servers.resources":          {},
	"mcp.servers.prompts":            {},
	"mcp.servers.refresh_discovery":  {},
	"mcp.servers.probe":              {},
	"mcp.servers.health":             {},
	"mcp.servers.bindings.list":      {},
	"mcp.servers.policy":             {},
	"mcp.servers.refresh_binding":    {},
	"mcp.servers.revoke_binding":     {},
	"mcp.servers.set_raw_html_trust": {},
	"mcp.servers.read_resource":      {},
	"mcp.apps.call_tool":             {},
	"mcp.apps.tool_context":          {},
	// Console-Tools-page cluster — seven methods.
	"tools.list":                {},
	"tools.get":                 {},
	"tools.describe":            {},
	"tools.metrics":             {},
	"tools.content_stats":       {},
	"tools.set_approval_policy": {},
	"tools.revoke_oauth":        {},
	// Console-Tasks-page cluster — two methods.
	"tasks.list": {},
	"tasks.get":  {},
	// Console-Sessions-page cluster — four methods (two reads + the
	// data-lifecycle erasure verb + the rename verb).
	"sessions.list":      {},
	"sessions.inspect":   {},
	"sessions.delete":    {},
	"sessions.set_title": {},
	// Session-turns read pair — the turn-projection surface (routes are
	// pinned explicitly; never derived generically).
	"sessions.turns.list": {},
	"sessions.turns.get":  {},
	// Observability administrative read — the one bounded rollup query.
	"observability.query": {},
	// Console-Playground-page cluster — one method.
	"runs.set_overrides": {},
	// State-snapshots cluster — one method (the windowed event-replay read).
	"state.history": {},
	// Events-read cluster — one method (the durable, time-ranged,
	// cross-session windowed raw-event read).
	"events.list": {},
	// Console-Settings-page cluster — one method.
	"auth.rotate_token": {},
	// Console-Flows-page cluster — six methods.
	"flows.list":          {},
	"flows.describe":      {},
	"flows.runs.list":     {},
	"flows.runs.describe": {},
	"flows.run":           {},
	"flows.metrics":       {},
	// Console-Agents-page cluster — eight methods.
	"agents.list":        {},
	"agents.get":         {},
	"agents.tools":       {},
	"agents.memory":      {},
	"agents.governance":  {},
	"agents.skills":      {},
	"agents.permissions": {},
	"agents.metrics":     {},
	// Console-Agents fleet-control verbs — five admin-
	// gated methods wrapping the in-process registry.* control verbs.
	"agents.pause":      {},
	"agents.drain":      {},
	"agents.restart":    {},
	"agents.force_stop": {},
	"agents.deregister": {},
}

// CanonicalWireTypes maps each canonical Protocol message struct type
// name to the single package directory (relative to the protocol-tree
// root) that is allowed to declare it. A declaration of one of these
// type names in ANY other package is a KindWireType violation.
//
// Almost every wire type lives in internal/protocol/types; the one
// exception is Error, the Protocol error wire type, which lives in
// internal/protocol/errors alongside the Code constants it carries
// (a settled carve-out: "internal/protocol/errors/errors.go ... the Error wire
// type"). Single-sourcing means "exactly one home", not "all in the
// same directory" — the map records the home per type.
//
// Version, Deprecation, and VersionHandshake are the
// versioning-discipline wire types — all in internal/protocol/types
// alongside the ProtocolVersion pin.
//
// Kept in lockstep with the canonical packages by
// TestSingleSource_CanonicalWireTypesInLockstep.
var CanonicalWireTypes = map[string]string{
	"IdentityScope":          "types",
	"StartRequest":           "types",
	"StartResponse":          "types",
	"ControlRequest":         "types",
	"ControlResponse":        "types",
	"Version":                "types",
	"Deprecation":            "types",
	"VersionHandshake":       "types",
	"EventFilter":            "types",
	"EventBucket":            "types",
	"EventAggregateRequest":  "types",
	"EventAggregateResponse": "types",
	"Error":                  "errors",
	// search cluster wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"SearchRequest":     "types",
	"SearchResponse":    "types",
	"SearchResultRow":   "types",
	"SearchFilter":      "types",
	"SearchFacet":       "types",
	"SearchArtifactRef": "types",
	// runtime-posture wire types — all live in
	// internal/protocol/types (internal/protocol/types/posture.go).
	"RuntimeInfoRequest":     "types",
	"RuntimeInfo":            "types",
	"ExternalGrantReadiness": "types",
	"SubsystemHealth":        "types",
	"RetentionHorizon":       "types",
	"RuntimeHealth":          "types",
	"RuntimeCounters":        "types",
	"SubsystemDriver":        "types",
	"RuntimeDrivers":         "types",
	"NamedCounter":           "types",
	"HistogramBucket":        "types",
	"NamedHistogram":         "types",
	"NamedGauge":             "types",
	"MetricsSnapshot":        "types",
	// posture-pair wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	// No `GovernancePostureRequest` / `LLMPostureRequest`: both methods take
	// the shared `RuntimeInfoRequest` envelope. The two request types were
	// orphans publishing a `tenant_id` field nothing decoded.
	"GovernancePostureResponse": "types",
	"IdentityTierView":          "types",
	"RateLimitView":             "types",
	"LLMPostureResponse":        "types",
	// Runtime-origin provider catalog operation results. These are bounded
	// admin Protocol responses and remain canonical wire shapes so the
	// consumer manifest cannot drift from Harbor's runtime surface.
	"LLMProviderOperationResponse":   "types",
	"LLMProviderDescriptor":          "types",
	"LLMProviderOperation":           "types",
	"LLMProviderValidation":          "types",
	"LLMProviderDiscovery":           "types",
	"LLMProviderOutcome":             "types",
	"LLMProviderModel":               "types",
	"LLMProviderCredentialField":     "types",
	"LLMProviderModelCapabilities":   "types",
	"LLMProviderNumericCapability":   "types",
	"LLMProviderSetCapability":       "types",
	"LLMProviderReasoningCapability": "types",
	"LLMProviderPricingCapability":   "types",
	// admin-scoped governance tenant-override wire types — the
	// `governance.{set,get}_tenant_overrides` request/response shapes
	// live in internal/protocol/types/governance.go (the RFC §6.15
	// ModelOverride seam).
	"GovernanceTenantOverrides":            "types",
	"GovernanceSetTenantOverridesRequest":  "types",
	"GovernanceSetTenantOverridesResponse": "types",
	"GovernanceGetTenantOverridesRequest":  "types",
	"GovernanceGetTenantOverridesResponse": "types",
	"GovernanceRotateKeyRequest":           "types",
	"GovernanceRotateKeyResponse":          "types",
	// admin-scoped governance identity-tier policy WRITE wire types — the
	// `governance.set_posture` request/response shapes (the write sibling of
	// `governance.posture`) live in internal/protocol/types/governance.go.
	"GovernanceSetPostureRequest":  "types",
	"GovernanceSetPostureResponse": "types",
	// agent-config control-plane wire types — the `agent_config.*` family
	// request/response shapes live in
	// internal/protocol/types/agentconfig.go (the agent-config registry primitive
	// + the admin-scope gate; skills control is the first consumer).
	"AgentConfigSkillsSelection":                    "types",
	"AgentConfigToolExposure":                       "types",
	"AgentConfigToolExposureDiff":                   "types",
	"AgentConfigLoadingModeChange":                  "types",
	"AgentConfigPromptLayers":                       "types",
	"AgentConfigNamedBlock":                         "types",
	"AgentConfigExtraSystemBlocks":                  "types",
	"AgentConfigExtraSystemBlocksDiff":              "types",
	"AgentConfigPromptLayersDiff":                   "types",
	"AgentConfigLLMParams":                          "types",
	"AgentConfigLLMParamsDiff":                      "types",
	"AgentConfigRunCompletionHook":                  "types",
	"AgentConfigHooks":                              "types",
	"AgentConfigHooksDiff":                          "types",
	"AgentConfigNaming":                             "types",
	"AgentConfigNamingDiff":                         "types",
	"AgentConfigPayload":                            "types",
	"AgentConfigRevisionView":                       "types",
	"AgentConfigSkillsDiff":                         "types",
	"AgentConfigDiff":                               "types",
	"AgentConfigGetRequest":                         "types",
	"AgentConfigGetResponse":                        "types",
	"AgentConfigSetRevisionRequest":                 "types",
	"AgentConfigSetRevisionResponse":                "types",
	"AgentConfigListRevisionsRequest":               "types",
	"AgentConfigListRevisionsResponse":              "types",
	"AgentConfigDiffRequest":                        "types",
	"AgentConfigDiffResponse":                       "types",
	"AgentConfigRollbackRequest":                    "types",
	"AgentConfigRollbackResponse":                   "types",
	"AgentConfigRetirementCleanupStep":              "types",
	"AgentConfigRetirementStatus":                   "types",
	"AgentConfigRetireRequest":                      "types",
	"AgentConfigRetireResponse":                     "types",
	"AgentConfigSkillSummary":                       "types",
	"AgentConfigSkillInput":                         "types",
	"AgentConfigSkillsListRequest":                  "types",
	"AgentConfigSkillsListResponse":                 "types",
	"AgentConfigSkillsUpsertRequest":                "types",
	"AgentConfigSkillsUpsertResponse":               "types",
	"AgentConfigSkillsDeleteRequest":                "types",
	"AgentConfigSkillsDeleteResponse":               "types",
	"AgentConfigAgentPackItem":                      "types",
	"AgentConfigAgentPacksListRequest":              "types",
	"AgentConfigAgentPacksListResponse":             "types",
	"AgentConfigAgentPacksUpsertRequest":            "types",
	"AgentConfigAgentPacksUpsertResponse":           "types",
	"AgentConfigAgentPacksRemoveRequest":            "types",
	"AgentConfigAgentPacksRemoveResponse":           "types",
	"AgentConfigAgentPacksProposeRequest":           "types",
	"AgentConfigAgentPacksProposeResponse":          "types",
	"AgentConfigAgentPacksCommitRequest":            "types",
	"AgentConfigAgentPacksCommitResponse":           "types",
	"AgentConfigSetToolExposureRequest":             "types",
	"AgentConfigSetToolExposureResponse":            "types",
	"AgentConfigSetPromptLayersRequest":             "types",
	"AgentConfigSetPromptLayersResponse":            "types",
	"AgentConfigSetExtraSystemBlocksRequest":        "types",
	"AgentConfigSetExtraSystemBlocksResponse":       "types",
	"AgentConfigSetLLMParamsRequest":                "types",
	"AgentConfigSetLLMParamsResponse":               "types",
	"AgentConfigMCPConnectionDescriptor":            "types",
	"AgentConfigMCPCredentialInjectionDescriptor":   "types",
	"AgentConfigConnections":                        "types",
	"AgentConfigConnectionsDiff":                    "types",
	"AgentConfigAddMCPConnectionRequest":            "types",
	"AgentConfigAddMCPConnectionResponse":           "types",
	"AgentConfigRemoveMCPConnectionRequest":         "types",
	"AgentConfigRemoveMCPConnectionResponse":        "types",
	"AgentConfigSetMCPDiscoveryOriginsRequest":      "types",
	"AgentConfigSetMCPDiscoveryOriginsResponse":     "types",
	"AgentConfigOAuthProviderDescriptor":            "types",
	"AgentConfigOAuthProviders":                     "types",
	"AgentConfigOAuthProvidersDiff":                 "types",
	"SignedOAuthMCPConnectionDescriptor":            "types",
	"AgentConfigSignedOAuthMCPPair":                 "types",
	"AgentConfigSetOAuthProviderRequest":            "types",
	"AgentConfigSetOAuthProviderResponse":           "types",
	"AgentConfigRegisterOAuthMCPCapabilityRequest":  "types",
	"AgentConfigRegisterOAuthMCPCapabilityResponse": "types",
	"AgentConfigRemoveOAuthMCPCapabilityRequest":    "types",
	"AgentConfigRemoveOAuthMCPCapabilityResponse":   "types",
	"AgentConfigRemoveOAuthProviderRequest":         "types",
	"AgentConfigRemoveOAuthProviderResponse":        "types",
	"AgentConfigLLMProviderDescriptor":              "types",
	"AgentConfigSetLLMProviderRequest":              "types",
	"AgentConfigSetLLMProviderResponse":             "types",
	// Session-user safe subset (the non-admin lower tier).
	"AgentConfigSessionOverlay":                   "types",
	"AgentConfigSessionSetUserPromptRequest":      "types",
	"AgentConfigSessionSetUserPromptResponse":     "types",
	"AgentConfigSessionSetSourceDisablesRequest":  "types",
	"AgentConfigSessionSetSourceDisablesResponse": "types",
	"AgentConfigSessionSkillsListRequest":         "types",
	"AgentConfigSessionSkillsListResponse":        "types",
	"AgentConfigSessionSkillsUpsertRequest":       "types",
	"AgentConfigSessionSkillsUpsertResponse":      "types",
	"AgentConfigSessionSkillsDeleteRequest":       "types",
	"AgentConfigSessionSkillsDeleteResponse":      "types",
	// user-scope durable config variant wire types (the middle tier).
	"AgentConfigUserPayload":               "types",
	"AgentConfigUserGetRequest":            "types",
	"AgentConfigUserGetResponse":           "types",
	"AgentConfigUserSetRevisionRequest":    "types",
	"AgentConfigUserSetRevisionResponse":   "types",
	"AgentConfigUserListRevisionsRequest":  "types",
	"AgentConfigUserListRevisionsResponse": "types",
	"AgentConfigUserDiffRequest":           "types",
	"AgentConfigUserDiffResponse":          "types",
	"AgentConfigUserRollbackRequest":       "types",
	"AgentConfigUserRollbackResponse":      "types",
	// Durable-per-user skills wire types (CLAIM-FREE).
	"AgentConfigUserSkillsListRequest":    "types",
	"AgentConfigUserSkillsListResponse":   "types",
	"AgentConfigUserSkillsUpsertRequest":  "types",
	"AgentConfigUserSkillsUpsertResponse": "types",
	"AgentConfigUserSkillsDeleteRequest":  "types",
	"AgentConfigUserSkillsDeleteResponse": "types",
	// Verified-caller skill-package import wire types (HA-61) — the
	// two-phase validate/commit family in internal/protocol/types
	// (internal/protocol/types/agentconfig.go).
	"AgentConfigUserSkillsImportValidateRequest":  "types",
	"AgentConfigUserSkillsImportValidateResponse": "types",
	"AgentConfigUserSkillImportSupportSummary":    "types",
	"AgentConfigUserSkillImportReview":            "types",
	"AgentConfigUserSkillsImportCommitRequest":    "types",
	"AgentConfigUserSkillImportReceipt":           "types",
	"AgentConfigUserSkillInstalledSummary":        "types",
	"AgentConfigUserSkillsImportCommitResponse":   "types",
	// Read-only composition-preview wire types (HA-66) — same home.
	"AgentConfigCompositionPreviewRequest":  "types",
	"AgentConfigCompositionPreviewItem":     "types",
	"AgentConfigCompositionPreviewResponse": "types",
	// pause-list snapshot wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"PauseListRequest":  "types",
	"PauseListResponse": "types",
	"PauseSnapshot":     "types",
	"PauseFilter":       "types",
	"PauseArtifactRef":  "types",
	// topology-projection wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"TopologyProjection":      "types",
	"TopologyNode":            "types",
	"TopologyEdge":            "types",
	"TopologySnapshotRequest": "types",
	// artifacts-page wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"ArtifactScope":           "types",
	"SizeRange":               "types",
	"TimeRange":               "types",
	"ArtifactRef":             "types",
	"ArtifactRow":             "types",
	"ArtifactsListRequest":    "types",
	"ArtifactsListResponse":   "types",
	"ArtifactsPutOpts":        "types",
	"ArtifactsPutRequest":     "types",
	"ArtifactsPutResponse":    "types",
	"ArtifactsGetRequest":     "types",
	"ArtifactsGetResponse":    "types",
	"ArtifactsGetRefRequest":  "types",
	"ArtifactsGetRefResponse": "types",
	"ArtifactsDeleteRequest":  "types",
	"ArtifactsDeleteResponse": "types",
	// Console-memory-page wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"MemoryItem":            "types",
	"MemoryFilter":          "types",
	"MemoryListRequest":     "types",
	"MemoryAggregates":      "types",
	"MemoryListResponse":    "types",
	"MemoryArtifactRef":     "types",
	"MemoryMetadata":        "types",
	"MemoryGetRequest":      "types",
	"MemoryItemDetail":      "types",
	"MemoryGetResponse":     "types",
	"MemoryHealthRequest":   "types",
	"MemoryHealthAggregate": "types",
	"MemoryHealthResponse":  "types",
	// strategy-trace + mutation wire types.
	"MemoryStrategyTraceRequest":  "types",
	"MemoryStrategyTrace":         "types",
	"MemoryStrategyTraceResponse": "types",
	"MemoryTurnInput":             "types",
	"MemoryPutRequest":            "types",
	"MemoryPutResponse":           "types",
	"MemoryDeleteRequest":         "types",
	"MemoryDeleteResponse":        "types",
	// MCP-Connections-page wire types — all live in
	// internal/protocol/types/mcp_servers.go. MCPServerStateView is a
	// string-enum type (like methods.Method / errors.Code) and is NOT
	// listed here — CanonicalWireTypes records struct wire types only.
	"MCPServerView":                     "types",
	"MCPOAuthRequirementView":           "types",
	"MCPScopeShortfallView":             "types",
	"MCPAuthorizationServerView":        "types",
	"MCPDiscoveryStepStatusView":        "types",
	"MCPServersListRequest":             "types",
	"MCPServersListResponse":            "types",
	"MCPServerGetRequest":               "types",
	"MCPToolPolicyView":                 "types",
	"MCPBindingScopeCount":              "types",
	"MCPServerGetResponse":              "types",
	"MCPResourceView":                   "types",
	"MCPServerResourcesRequest":         "types",
	"MCPServerResourcesResponse":        "types",
	"MCPPromptArg":                      "types",
	"MCPPromptView":                     "types",
	"MCPServerPromptsRequest":           "types",
	"MCPServerPromptsResponse":          "types",
	"MCPServerRefreshDiscoveryRequest":  "types",
	"MCPServerRefreshDiscoveryResponse": "types",
	"MCPServerProbeRequest":             "types",
	"MCPServerProbeResponse":            "types",
	"MCPHealthBucket":                   "types",
	"MCPReconnect":                      "types",
	"MCPServerHealthRequest":            "types",
	"MCPServerHealthResponse":           "types",
	"MCPBindingView":                    "types",
	"MCPServerBindingsListRequest":      "types",
	"MCPServerBindingsListResponse":     "types",
	"MCPServerPolicyRequest":            "types",
	"MCPServerPolicyResponse":           "types",
	"MCPServerRefreshBindingRequest":    "types",
	"MCPServerRefreshBindingResponse":   "types",
	"MCPServerRevokeBindingRequest":     "types",
	"MCPServerRevokeBindingResponse":    "types",
	"MCPServerSetRawHTMLTrustRequest":   "types",
	"MCPServerSetRawHTMLTrustResponse":  "types",
	// MCP Apps host wire types — the `ui://` document fetch + the
	// app-tool-call proxy, all in internal/protocol/types/mcp_apps.go.
	"ReadMCPResourceRequest":  "types",
	"ReadMCPResourceResponse": "types",
	"MCPResourceArtifactRef":  "types",
	"MCPAppCallToolRequest":   "types",
	"MCPAppCallToolResponse":  "types",
	"MCPAppRef":               "types",
	"ToolContextRequest":      "types",
	"ToolContextPayload":      "types",
	"ToolContextResponse":     "types",
	// render-admission wire type (the HA-56 amendment) — same home.
	"RenderAdmission": "types",
	// Console-Tools-page wire types — all live in
	// internal/protocol/types (internal/protocol/types/tools.go).
	"Tool":                          "types",
	"ToolFilter":                    "types",
	"ToolListRequest":               "types",
	"ToolListResponse":              "types",
	"ToolAggregates":                "types",
	"ToolGetRequest":                "types",
	"ToolDescribeRequest":           "types",
	"ToolManifest":                  "types",
	"ToolMetricsRequest":            "types",
	"ToolMetrics":                   "types",
	"ToolContentStatsRequest":       "types",
	"ToolContentStats":              "types",
	"ToolContentBucket":             "types",
	"ToolSetApprovalPolicyRequest":  "types",
	"ToolSetApprovalPolicyResponse": "types",
	"ToolRevokeOAuthRequest":        "types",
	"ToolRevokeOAuthResponse":       "types",
	// Console Tasks-page wire types — all live in
	// internal/protocol/types (internal/protocol/types/tasks.go).
	// Harbor extends the cluster with the Live Runtime
	// header status-counter-strip aggregate.
	"TasksListStatusCounterStrip": "types",
	"TaskRow":                     "types",
	"TaskFilter":                  "types",
	"TaskListAggregates":          "types",
	"TaskListCursor":              "types",
	"TaskListRequest":             "types",
	"TaskListResponse":            "types",
	"TaskDetail":                  "types",
	"TaskParentSessionRef":        "types",
	"TaskParentTaskRef":           "types",
	"TaskCostRollup":              "types",
	"TaskCostStep":                "types",
	"TaskPlannerSnapshotRef":      "types",
	"TaskGetRequest":              "types",
	// TaskDetail.Trajectory wire types.
	"TaskTrajectoryRef":  "types",
	"TaskTrajectoryStep": "types",
	// per-attachment disposition on tasks.get.
	"TaskInputArtifact":    "types",
	"TaskProgressSnapshot": "types",
	// Console Flows-page wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"Flow":                   "types",
	"FlowBudget":             "types",
	"FlowBudgetConsumption":  "types",
	"FlowFilter":             "types",
	"FlowListRequest":        "types",
	"FlowListResponse":       "types",
	"FlowNode":               "types",
	"FlowNodePolicy":         "types",
	"FlowEdge":               "types",
	"FlowDescription":        "types",
	"FlowDescribeRequest":    "types",
	"FlowRun":                "types",
	"FlowRunsListRequest":    "types",
	"FlowRunsListResponse":   "types",
	"FlowArtifactRef":        "types",
	"FlowNodeRunState":       "types",
	"FlowRunDescription":     "types",
	"FlowRunDescribeRequest": "types",
	"FlowRunRequest":         "types",
	"FlowRunResponse":        "types",
	"FlowMetricsBucket":      "types",
	"FlowMetrics":            "types",
	"FlowMetricsRequest":     "types",
	// Console Agents-page wire types — all live in
	// internal/protocol/types (internal/protocol/types/agents.go).
	// AgentStatus / AgentHealth / AgentHosting are string-enum types
	// (like methods.Method / errors.Code) and are NOT listed here —
	// CanonicalWireTypes records struct wire types only.
	"Agent":                    "types",
	"AgentFilter":              "types",
	"AgentListRequest":         "types",
	"AgentAggregates":          "types",
	"AgentListResponse":        "types",
	"AgentGetRequest":          "types",
	"AgentConfig":              "types",
	"AgentGetResponse":         "types",
	"AgentToolsRequest":        "types",
	"AgentToolBinding":         "types",
	"AgentToolsResponse":       "types",
	"AgentMemoryRequest":       "types",
	"AgentMemoryBinding":       "types",
	"AgentMemoryResponse":      "types",
	"AgentGovernanceRequest":   "types",
	"AgentCostCeiling":         "types",
	"AgentRateLimit":           "types",
	"AgentGovernance":          "types",
	"AgentGovernanceResponse":  "types",
	"AgentSkillsRequest":       "types",
	"AgentSkillBinding":        "types",
	"AgentSkillsResponse":      "types",
	"AgentPermissionsRequest":  "types",
	"AgentPermissions":         "types",
	"AgentPermissionsResponse": "types",
	"AgentMetricsRequest":      "types",
	"AgentMetrics":             "types",
	"AgentMetricsResponse":     "types",
	// fleet-control wire types — the shared control
	// request/response (internal/protocol/types/agents.go).
	"AgentControlRequest":  "types",
	"AgentControlResponse": "types",
	// Console Sessions-page wire types — all live in
	// internal/protocol/types (internal/protocol/types/sessions.go).
	// SessionStatus / SessionSort are string-enum types (like
	// methods.Method / errors.Code) and are NOT listed here —
	// CanonicalWireTypes records struct wire types only.
	"Window":                   "types",
	"SessionFilter":            "types",
	"SessionsListRequest":      "types",
	"SessionRow":               "types",
	"SessionsListResponse":     "types",
	"InterventionSummary":      "types",
	"ArtifactRefSummary":       "types",
	"SessionsInspectRequest":   "types",
	"SessionsInspectResponse":  "types",
	"SessionsDeleteRequest":    "types",
	"SessionsDeleteResponse":   "types",
	"SessionsSetTitleRequest":  "types",
	"SessionsSetTitleResponse": "types",
	// Session-turns wire types (HA-63/64) — the turn-projection read
	// pair, all in internal/protocol/types/session_turns.go.
	"SessionTurnsListRequest":   "types",
	"SessionTurnHeader":         "types",
	"SessionTurnsListResponse":  "types",
	"SessionTurnsGetRequest":    "types",
	"SessionTurnsGetResponse":   "types",
	"SessionTurnRow":            "types",
	"SessionTurnAgent":          "types",
	"SessionTurnQuery":          "types",
	"SessionTurnAnswer":         "types",
	"SessionTurnAnswerRef":      "types",
	"SessionTurnPause":          "types",
	"SessionTurnAttachment":     "types",
	"SessionTurnUsage":          "types",
	"SessionTurnUsageMeasure":   "types",
	"SessionTurnReasoning":      "types",
	"SessionTurnReasoningStep":  "types",
	"SessionTurnActivity":       "types",
	"SessionTurnActivityTotals": "types",
	"SessionTurnActivityRow":    "types",
	"SessionTurnAppRef":         "types",
	"SessionOpsTurnRow":         "types",
	"SessionOpsAppRef":          "types",
	// Observability wire types (HA-65) — the one bounded administrative
	// rollup query, all in internal/protocol/types/observability.go.
	"ObservabilityQueryFilter":   "types",
	"ObservabilityQueryRequest":  "types",
	"ObservabilityMeasureValue":  "types",
	"ObservabilityQueryRow":      "types",
	"ObservabilityQualityBlock":  "types",
	"ObservabilityQueryResponse": "types",
	// Console Playground-page wire types — all live
	// in internal/protocol/types (internal/protocol/types/runs.go).
	"RunOverrides":            "types",
	"RunSetOverridesRequest":  "types",
	"RunSetOverridesResponse": "types",
	// Console Settings-page wire types — the
	// `auth.rotate_token` request / response live in
	// internal/protocol/types (internal/protocol/types/auth.go). The
	// ONE net-new Protocol method Harbor ships; the page is
	// otherwise a pure consumer of the posture surfaces.
	"AuthRotateTokenRequest":  "types",
	"AuthRotateTokenResponse": "types",
	// State-snapshots windowed event-replay wire types — the
	// `state.history` request/response + the flat routable artifact ref +
	// the flat event projection live in
	// internal/protocol/types (internal/protocol/types/state.go).
	"StateHistoryRequest":  "types",
	"StateArtifactRef":     "types",
	"StateEvent":           "types",
	"StateHistoryResponse": "types",
	// Events-read windowed raw-event wire types — the `events.list`
	// request/response reuse the existing EventFilter + StateEvent row
	// (no new row shape) and live in internal/protocol/types
	// (internal/protocol/types/events.go).
	"EventsListRequest":  "types",
	"EventsListResponse": "types",
}

var skillPublicationCanonicalWireTypes = map[string]string{
	"SkillPublicationMetadata":               "types",
	"SkillPublicationReference":              "types",
	"SkillPublicationReceipt":                "types",
	"SkillPublicationPublishRequest":         "types",
	"SkillPublicationPublishResponse":        "types",
	"SkillPublicationListRequest":            "types",
	"SkillPublicationListResponse":           "types",
	"SkillPublicationGetRequest":             "types",
	"SkillPublicationGetResponse":            "types",
	"SkillPublicationSuccessorRequest":       "types",
	"SkillPublicationSuccessorResponse":      "types",
	"SkillPublicationRetireRequest":          "types",
	"SkillPublicationRetireResponse":         "types",
	"SkillPublicationAvailableRequest":       "types",
	"SkillPublicationAvailableResponse":      "types",
	"SkillPublicationInstallRequest":         "types",
	"SkillPublicationInstallResponse":        "types",
	"SkillPublicationUpdateRequest":          "types",
	"SkillPublicationUpdateResponse":         "types",
	"SkillPublicationRemoveRequest":          "types",
	"SkillPublicationRemoveResponse":         "types",
	"SkillPublicationReferencesListRequest":  "types",
	"SkillPublicationReferencesListResponse": "types",
}

func init() {
	for name, home := range skillPublicationCanonicalWireTypes {
		CanonicalWireTypes[name] = home
	}
}

// dirAllowsKind reports whether the package directory dir (a path
// relative to the protocol-tree root, slash-separated) is the canonical
// home for the given Violation kind — i.e. the kind is permitted there.
// It covers the kinds with a single fixed home (method literals,
// error codes); KindWireType has a per-type home and is gated by
// CanonicalWireTypes directly.
func dirAllowsKind(dir, kind string) bool {
	switch kind {
	case KindMethodLiteral:
		return dir == "methods"
	case KindErrorCode:
		return dir == "errors"
	default:
		return false
	}
}

// ScanProtocolTree walks the Go source tree rooted at protocolRoot
// (expected to be the internal/protocol directory) and returns every
// single-source Violation it finds. It parses .go files — including
// _test.go files, because a method string hardcoded in a test is the
// same drift as one hardcoded in production — with go/parser, so the
// check is precise: a method name inside a comment, a doc string, or a
// struct tag is NOT flagged; only a real string-literal expression, a
// real const declaration, or a real type declaration is.
//
// protocolRoot may be absolute or relative; reported Violation.File
// paths are slash-separated and relative to protocolRoot either way.
//
// The scan is exhaustive (it returns ALL violations, not the first) and
// deterministic (violations are sorted by file then line). It has no
// package-level mutable state and is safe for concurrent use.
//
// A returned error means the walk itself failed (an unreadable file, an
// unparseable source file) — that is distinct from a Violation, which
// is a successful scan finding drift.
func ScanProtocolTree(protocolRoot string) ([]Violation, error) {
	fset := token.NewFileSet()
	var violations []Violation

	walkErr := filepath.WalkDir(protocolRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Skip vendored / build artefacts and the checker's own
			// package — singlesource.go necessarily mentions the
			// canonical method strings (in CanonicalMethods) and the
			// canonical type names (in CanonicalWireTypes); it is the
			// audit tool, not a Protocol-definition site.
			if name == "vendor" || name == "testdata" || name == "singlesource" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(protocolRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativise %q: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if pkgDir == "." {
			pkgDir = ""
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %q: %w", rel, parseErr)
		}

		violations = append(violations, scanFile(fset, file, rel, pkgDir)...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk protocol tree %q: %w", protocolRoot, walkErr)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})
	return violations, nil
}

// scanFile walks a single parsed file's AST and collects the three
// single-source violation kinds. pkgDir is the file's package directory
// relative to the protocol-tree root ("" for the root package,
// "methods" / "errors" / "types" / ... for sub-packages).
func scanFile(fset *token.FileSet, file *ast.File, rel, pkgDir string) []Violation {
	var out []Violation

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			// A STRING literal whose unquoted value is a canonical
			// Protocol method name is a hardcoded method string. Allowed
			// only inside internal/protocol/methods.
			if node.Kind != token.STRING {
				return true
			}
			if dirAllowsKind(pkgDir, KindMethodLiteral) {
				return true
			}
			val, uErr := strconv.Unquote(node.Value)
			if uErr != nil {
				return true
			}
			if _, isMethod := CanonicalMethods[val]; isMethod {
				out = append(out, Violation{
					File: rel,
					Line: fset.Position(node.Pos()).Line,
					Kind: KindMethodLiteral,
					Detail: fmt.Sprintf(
						"hardcoded Protocol method string %q — method names are single-sourced in internal/protocol/methods (use the methods.Method* constant)",
						val),
				})
			}

		case *ast.TypeSpec:
			// A `type Error ...` / `type StartRequest ...` declaration
			// of a canonical Protocol wire type outside its single home
			// package redefines a single-sourced type.
			name := node.Name.Name
			home, isWireType := CanonicalWireTypes[name]
			if !isWireType {
				return true
			}
			if pkgDir == home {
				return true
			}
			out = append(out, Violation{
				File: rel,
				Line: fset.Position(node.Name.Pos()).Line,
				Kind: KindWireType,
				Detail: fmt.Sprintf(
					"redeclared canonical Protocol wire type %q — it is single-sourced in internal/protocol/%s",
					name, home),
			})

		case *ast.GenDecl:
			// A const declaration whose type is the protocol/errors.Code
			// type, made outside internal/protocol/errors, is a second
			// definition site for Protocol error codes.
			if node.Tok != token.CONST {
				return true
			}
			if dirAllowsKind(pkgDir, KindErrorCode) {
				return true
			}
			for _, spec := range node.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				if !isProtocolErrorsCodeType(vs.Type) {
					continue
				}
				for _, ident := range vs.Names {
					out = append(out, Violation{
						File: rel,
						Line: fset.Position(ident.Pos()).Line,
						Kind: KindErrorCode,
						Detail: fmt.Sprintf(
							"declared Protocol error code constant %q of type protocol/errors.Code — error codes are single-sourced in internal/protocol/errors",
							ident.Name),
					})
				}
			}
		}
		return true
	})

	return out
}

// isProtocolErrorsCodeType reports whether the type expression names the
// protocol/errors.Code type — either the bare `Code` identifier (inside
// the errors package itself, which dirAllowsKind already excludes from
// the scan) or a `<pkg>.Code` selector where the selector base resolves
// to the protocol errors package. The check is intentionally
// conservative: it matches a `.Code` selector on ANY package alias, then
// the scan's pkgDir gate ensures the errors package itself is never
// flagged. A const of an unrelated `Code` type in a non-errors package
// would be a false positive, so the detail message names the type to
// make a false positive obvious in review — but no such type exists in
// the Protocol tree, and TestSingleSource_NoFalsePositiveOnNonProtocolCode
// pins that.
func isProtocolErrorsCodeType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		// `protoerrors.Code`, `errors.Code`, etc. — a selector ending in
		// the identifier `Code`.
		return t.Sel.Name == "Code"
	case *ast.Ident:
		// A bare `Code` — only reachable inside the errors package,
		// which dirAllowsKind already excludes before this is called.
		return t.Name == "Code"
	default:
		return false
	}
}
