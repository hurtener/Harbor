package bodyscope

import "sort"

// requestSurfaces joins every canonical Protocol REQUEST type that
// carries a body identity scope to the surface whose policy governs it.
//
// This table is the coverage half of the gate. The universe it must
// cover is derived mechanically: the lockstep test parses
// internal/protocol/types, collects every exported struct whose name ends
// in "Request" and which carries a types.IdentityScope or
// types.ArtifactScope field, and requires a row here for each. It also
// runs the check in reverse, so a row whose request type was renamed or
// deleted fails just as loudly as a request type with no row.
//
// The effect is that a NEW Protocol request type carrying an identity
// scope cannot ship without someone answering "which surface's
// body-identity posture governs this?" — and answering it in the one
// table a reviewer reads, rather than in a freshly-copied helper.
var requestSurfaces = map[string]Surface{
	// agent_config — the versioned desired-state spine and its
	// per-user / per-session variants.
	"AgentConfigAddMCPConnectionRequest":           SurfaceAgentConfig,
	"AgentConfigDiffRequest":                       SurfaceAgentConfig,
	"AgentConfigGetRequest":                        SurfaceAgentConfig,
	"AgentConfigListRevisionsRequest":              SurfaceAgentConfig,
	"AgentConfigRemoveMCPConnectionRequest":        SurfaceAgentConfig,
	"AgentConfigRemoveOAuthMCPCapabilityRequest":   SurfaceAgentConfig,
	"AgentConfigRemoveOAuthProviderRequest":        SurfaceAgentConfig,
	"AgentConfigRetireRequest":                     SurfaceAgentConfig,
	"AgentConfigRollbackRequest":                   SurfaceAgentConfig,
	"AgentConfigSessionSetSourceDisablesRequest":   SurfaceAgentConfig,
	"AgentConfigSessionSetUserPromptRequest":       SurfaceAgentConfig,
	"AgentConfigSessionSkillsDeleteRequest":        SurfaceAgentConfig,
	"AgentConfigSessionSkillsListRequest":          SurfaceAgentConfig,
	"AgentConfigSessionSkillsUpsertRequest":        SurfaceAgentConfig,
	"AgentConfigSetLLMParamsRequest":               SurfaceAgentConfig,
	"AgentConfigSetLLMProviderRequest":             SurfaceAgentConfig,
	"AgentConfigSetMCPDiscoveryOriginsRequest":     SurfaceAgentConfig,
	"AgentConfigSetOAuthProviderRequest":           SurfaceAgentConfig,
	"AgentConfigRegisterOAuthMCPCapabilityRequest": SurfaceAgentConfig,
	"AgentConfigSetExtraSystemBlocksRequest":       SurfaceAgentConfig,
	"AgentConfigSetPromptLayersRequest":            SurfaceAgentConfig,
	"AgentConfigSetRevisionRequest":                SurfaceAgentConfig,
	"AgentConfigSetToolExposureRequest":            SurfaceAgentConfig,
	"AgentConfigSkillsDeleteRequest":               SurfaceAgentConfig,
	"AgentConfigSkillsListRequest":                 SurfaceAgentConfig,
	"AgentConfigSkillsUpsertRequest":               SurfaceAgentConfig,
	"AgentConfigAgentPacksListRequest":             SurfaceAgentConfig,
	"AgentConfigAgentPacksUpsertRequest":           SurfaceAgentConfig,
	"AgentConfigAgentPacksRemoveRequest":           SurfaceAgentConfig,
	"AgentConfigAgentPacksProposeRequest":          SurfaceAgentConfig,
	"AgentConfigAgentPacksCommitRequest":           SurfaceAgentConfig,
	"AgentConfigUserDiffRequest":                   SurfaceAgentConfig,
	"AgentConfigUserGetRequest":                    SurfaceAgentConfig,
	"AgentConfigUserListRevisionsRequest":          SurfaceAgentConfig,
	"AgentConfigUserRollbackRequest":               SurfaceAgentConfig,
	"AgentConfigUserSetRevisionRequest":            SurfaceAgentConfig,
	"AgentConfigUserSkillsDeleteRequest":           SurfaceAgentConfig,
	"AgentConfigUserSkillsListRequest":             SurfaceAgentConfig,
	"AgentConfigUserSkillsUpsertRequest":           SurfaceAgentConfig,

	// agents — the registry page.
	"AgentControlRequest":     SurfaceAgents,
	"AgentGetRequest":         SurfaceAgents,
	"AgentGovernanceRequest":  SurfaceAgents,
	"AgentListRequest":        SurfaceAgents,
	"AgentMemoryRequest":      SurfaceAgents,
	"AgentMetricsRequest":     SurfaceAgents,
	"AgentPermissionsRequest": SurfaceAgents,
	"AgentSkillsRequest":      SurfaceAgents,
	"AgentToolsRequest":       SurfaceAgents,

	// artifacts — the one surface whose list and put reach another
	// tenant under the admin claim. The two CONTENT reads join the SAME
	// flat row: a driver-independent byte read and a presigned reference
	// hand over the same thing over different transports, so the posture
	// that refuses one refuses the other, and it is one row rather than
	// two copies of one reason.
	"ArtifactsDeleteRequest": SurfaceArtifactsDelete,
	"ArtifactsGetRequest":    SurfaceArtifactsRef,
	"ArtifactsGetRefRequest": SurfaceArtifactsRef,
	"ArtifactsListRequest":   SurfaceArtifacts,
	"ArtifactsPutRequest":    SurfaceArtifactsPut,

	// auth.
	"AuthRotateTokenRequest": SurfaceAuth,

	// task — start and the steering controls.
	"ControlRequest": SurfaceControlTask,
	"StartRequest":   SurfaceControlTask,

	// events.
	"EventAggregateRequest": SurfaceEvents,
	"EventsListRequest":     SurfaceEvents,

	// flows.
	"FlowDescribeRequest":    SurfaceFlows,
	"FlowListRequest":        SurfaceFlows,
	"FlowMetricsRequest":     SurfaceFlows,
	"FlowRunDescribeRequest": SurfaceFlows,
	"FlowRunRequest":         SurfaceFlows,
	"FlowRunsListRequest":    SurfaceFlows,

	// governance.
	"GovernanceGetTenantOverridesRequest": SurfaceGovernance,
	"GovernanceRotateKeyRequest":          SurfaceGovernance,
	"GovernanceSetTenantOverridesRequest": SurfaceGovernance,

	// mcp.apps — the rendered-app host surface.
	"MCPAppCallToolRequest":  SurfaceApps,
	"ReadMCPResourceRequest": SurfaceApps,
	"ToolContextRequest":     SurfaceApps,

	// mcp.servers — the connection catalog.
	"MCPServerBindingsListRequest":     SurfaceMCP,
	"MCPServerGetRequest":              SurfaceMCP,
	"MCPServerHealthRequest":           SurfaceMCP,
	"MCPServerPolicyRequest":           SurfaceMCP,
	"MCPServerProbeRequest":            SurfaceMCP,
	"MCPServerPromptsRequest":          SurfaceMCP,
	"MCPServerRefreshBindingRequest":   SurfaceMCP,
	"MCPServerRefreshDiscoveryRequest": SurfaceMCP,
	"MCPServerResourcesRequest":        SurfaceMCP,
	"MCPServerRevokeBindingRequest":    SurfaceMCP,
	"MCPServerSetRawHTMLTrustRequest":  SurfaceMCP,
	"MCPServersListRequest":            SurfaceMCP,

	// memory.
	"MemoryDeleteRequest":        SurfaceMemory,
	"MemoryGetRequest":           SurfaceMemory,
	"MemoryHealthRequest":        SurfaceMemory,
	"MemoryListRequest":          SurfaceMemory,
	"MemoryPutRequest":           SurfaceMemory,
	"MemoryStrategyTraceRequest": SurfaceMemory,

	// pause.
	"PauseListRequest": SurfacePause,

	// runtime posture.
	"RuntimeInfoRequest": SurfacePosture,

	// runs.
	"RunSetOverridesRequest": SurfaceRuns,

	// sessions.
	"SessionsDeleteRequest":   SurfaceSessions,
	"SessionsInspectRequest":  SurfaceSessions,
	"SessionsListRequest":     SurfaceSessions,
	"SessionsSetTitleRequest": SurfaceSessions,

	// state.history.
	"StateHistoryRequest": SurfaceStateHistory,

	// tasks.
	"TaskGetRequest":  SurfaceTasks,
	"TaskListRequest": SurfaceTasks,

	// tools.
	"ToolContentStatsRequest":      SurfaceTools,
	"ToolDescribeRequest":          SurfaceTools,
	"ToolGetRequest":               SurfaceTools,
	"ToolListRequest":              SurfaceTools,
	"ToolMetricsRequest":           SurfaceTools,
	"ToolRevokeOAuthRequest":       SurfaceTools,
	"ToolSetApprovalPolicyRequest": SurfaceTools,

	// topology.
	"TopologySnapshotRequest": SurfaceTopology,
}

// nonRequestScopeCarriers are the canonical wire types that carry an
// identity scope but are NOT request bodies — response rows, snapshots
// and projections the runtime writes and the caller reads. They are
// exempt from the join because there is no caller-supplied triple to
// reconcile: the runtime authored the value.
//
// The list is explicit so the exemption is reviewed rather than inferred
// from a naming convention. The lockstep test pins it in both directions:
// a new scope-carrying type that is neither a joined request nor a listed
// carrier fails, and a listed carrier that no longer exists fails too.
var nonRequestScopeCarriers = map[string]string{
	"Agent":         "registry projection row; the identity is the registered agent's own",
	"ArtifactRef":   "artifact metadata projection; the scope is the stored artifact's own",
	"FlowRun":       "flow-run projection row; the identity is the recorded run's own",
	"MemoryItem":    "memory record projection; the identity is the stored record's own",
	"PauseSnapshot": "pause-record projection; the identity is the paused run's own",
	"SessionRow":    "session listing row; the identity is the listed session's own",
	"TaskRow":       "task listing row; the identity is the listed task's own",
}

// SurfaceForRequest returns the Surface governing the named canonical
// request type and a presence bool.
func SurfaceForRequest(requestType string) (Surface, bool) {
	s, ok := requestSurfaces[requestType]
	return s, ok
}

// JoinedRequestTypes returns every canonical request type with a
// registered surface, sorted.
func JoinedRequestTypes() []string {
	out := make([]string, 0, len(requestSurfaces))
	for t := range requestSurfaces {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ExemptScopeCarriers returns the non-request scope-carrying wire types
// and the reason each is exempt from the join.
func ExemptScopeCarriers() map[string]string {
	out := make(map[string]string, len(nonRequestScopeCarriers))
	for t, why := range nonRequestScopeCarriers {
		out[t] = why
	}
	return out
}
