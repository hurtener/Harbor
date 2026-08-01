package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/devdraft"
	"github.com/hurtener/Harbor/internal/distributed" // payload home for distributed.bus_envelope (the shared bus-projection contract; prod already blank-imports the drivers)
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	mcptool "github.com/hurtener/Harbor/internal/tools/drivers/mcp" // payload home for mcp.resource_updated (the MCP southbound driver declares it; imported only to read the exported type)

	// The Q1 registry-read (proposal §3.1, resolved in the
	// plan): blank-importing the §4.4 production aggregator transitively
	// seats every subsystem's RegisterEventType init() before the
	// generator reads events.EventTypes(). The payload-type imports
	// above seat the remaining (non-aggregated) registrations — the
	// dev-only draft topic, the Protocol-edge topics, the MCP southbound
	// driver — so the catalog is the full set every Harbor binary can
	// publish.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
)

// payloadEntry records what a subscriber receives for one canonical
// event type: the typed payload struct(s) the emitting subsystem
// publishes (several when distinct emit sites publish distinct shapes,
// e.g. `audit.admin_scope_used`), or — when Payloads is empty — a Note
// explaining why no typed shape exists.
type payloadEntry struct {
	// Payloads are the declared payload struct types, rendered
	// field-by-field. Empty means "no typed payload" and Note explains.
	Payloads []reflect.Type
	// Note is appended to the section prose (mandatory when Payloads is
	// empty).
	Note string
}

// eventPayloadIndex maps EVERY canonical event type to its payload
// shape(s). It is the events-page sibling of singlesource's
// CanonicalWireTypes treatment: the registry owns the type SET (read at
// gen time via the prod-aggregator import — proposal Q1), this index
// owns the payload JOIN, and TestGen_EventPayloadIndexInLockstep fails
// the build when events.EventTypes() gains an entry the index does not
// carry (or the index names an unregistered type) — the exact mechanism
// TestSingleSource_CanonicalMethodsInLockstep uses for the method set.
var eventPayloadIndex = map[events.EventType]payloadEntry{
	// --- Bus-internal + core runtime types (internal/events).
	events.EventTypeRuntimeError:              {Payloads: []reflect.Type{reflect.TypeOf(events.RuntimeErrorPayload{})}},
	events.EventTypeRuntimeWarning:            {Note: "Reserved for future runtime-warn emits — registered, no production Runtime emit path in this version; published only by the Protocol conformance suite as a driver-certification fixture."},
	events.EventTypeBusDropped:                {Payloads: []reflect.Type{reflect.TypeOf(events.BusDroppedPayload{})}},
	events.EventTypeBusSubscriptionIdleClosed: {Payloads: []reflect.Type{reflect.TypeOf(events.SubscriptionIdleClosedPayload{})}},
	events.EventTypeAuditRedactionFailed:      {Payloads: []reflect.Type{reflect.TypeOf(events.AuditRedactionFailedPayload{})}},
	events.EventTypeRuntimeRunCancelled:       {Payloads: []reflect.Type{reflect.TypeOf(events.RunCancelledPayload{})}},
	events.EventTypeTopologyChanged:           {Payloads: []reflect.Type{reflect.TypeOf(events.TopologyChangedPayload{})}},
	events.EventTypeMCPRawHTMLTrustToggled:    {Payloads: []reflect.Type{reflect.TypeOf(events.MCPRawHTMLTrustToggledPayload{})}},
	events.EventTypeRunOverridesSet:           {Payloads: []reflect.Type{reflect.TypeOf(events.RunOverridesSetPayload{})}},
	events.EventTypeAdminScopeUsed:            {Payloads: []reflect.Type{reflect.TypeOf(events.AdminScopeUsedPayload{}), reflect.TypeOf(protoauth.AdminScopeUsedPayload{})}, Note: "Two emit sites publish distinct shapes: the bus's admin-filter Subscribe emit (first shape) and the Protocol-edge admin/impersonation audit emit (second shape)."},
	events.EventTypeGovernanceBudgetExceeded:  {Payloads: []reflect.Type{reflect.TypeOf(governance.BudgetExceededPayload{})}},
	events.EventTypeGovernanceRateLimited:     {Payloads: []reflect.Type{reflect.TypeOf(governance.RateLimitedPayload{})}},
	governance.EventTypeMaxTokensExceeded:     {Payloads: []reflect.Type{reflect.TypeOf(governance.MaxTokensExceededPayload{})}},
	governance.EventTypePostureReadAdmin:      {Payloads: []reflect.Type{reflect.TypeOf(governance.PostureReadAdminPayload{})}},
	governance.EventTypeTenantOverridesSet:    {Payloads: []reflect.Type{reflect.TypeOf(governance.TenantOverridesSetPayload{})}},
	governance.EventTypeKeyRotated:            {Payloads: []reflect.Type{reflect.TypeOf(governance.KeyRotatedPayload{})}},
	governance.EventTypePostureSet:            {Payloads: []reflect.Type{reflect.TypeOf(governance.GovernancePostureSetPayload{})}},
	governance.EventTypeFailover:              {Payloads: []reflect.Type{reflect.TypeOf(governance.GovernanceFailoverPayload{})}},

	// --- Agent-config control plane (internal/agentcfg).
	agentcfg.EventTypeConfigRevised:               {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.ConfigRevisedPayload{})}},
	agentcfg.EventTypeConfigReverted:              {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.ConfigRevertedPayload{})}},
	agentcfg.EventTypeMCPConnectionPaused:         {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionPausedPayload{})}},
	agentcfg.EventTypeMCPConnectionResumed:        {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionResumedPayload{})}},
	agentcfg.EventTypeMCPConnectionPending:        {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPConnectionAdded:          {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPConnectionFailed:         {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPConnectionAuthRequired:   {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPConnectionRemoved:        {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionRemovedPayload{})}},
	agentcfg.EventTypeMCPConnectionReattached:     {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPConnectionReattachFailed: {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPConnectionLifecyclePayload{})}},
	agentcfg.EventTypeMCPDiscoveryOriginsSet:      {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.MCPDiscoveryOriginsSetPayload{})}},
	agentcfg.EventTypeOAuthProviderInstalled:      {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.OAuthProviderSetPayload{})}},
	agentcfg.EventTypeOAuthProviderRemoved:        {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.OAuthProviderSetPayload{})}},
	agentcfg.EventTypeLLMProviderInstalled:        {Payloads: []reflect.Type{reflect.TypeOf(agentcfg.LLMProviderSetPayload{})}},

	// --- Dev-draft lifecycle (harbor dev's dynamic agent scaffolding).
	devdraft.EventTypeDraftCreated:   {Payloads: []reflect.Type{reflect.TypeOf(devdraft.DraftCreatedPayload{})}},
	devdraft.EventTypeDraftUpdated:   {Payloads: []reflect.Type{reflect.TypeOf(devdraft.DraftUpdatedPayload{})}},
	devdraft.EventTypeDraftPreviewed: {Payloads: []reflect.Type{reflect.TypeOf(devdraft.DraftPreviewedPayload{})}},
	devdraft.EventTypeDraftSaved:     {Payloads: []reflect.Type{reflect.TypeOf(devdraft.DraftSavedPayload{})}},
	devdraft.EventTypeDraftDiscarded: {Payloads: []reflect.Type{reflect.TypeOf(devdraft.DraftDiscardedPayload{})}},

	// --- Distributed bus.
	distributed.EventTypeDistributedBusEnvelope: {Payloads: []reflect.Type{reflect.TypeOf(distributed.BusEnvelopePayload{})}},

	// --- LLM edge.
	llm.EventTypeImageMaterialized:         {Payloads: []reflect.Type{reflect.TypeOf(llm.ImageMaterializedPayload{})}},
	llm.EventTypeContextLeak:               {Payloads: []reflect.Type{reflect.TypeOf(llm.ContextLeakPayload{})}},
	llm.EventTypeContextWindowExceeded:     {Payloads: []reflect.Type{reflect.TypeOf(llm.ContextWindowExceededPayload{})}},
	llm.EventTypeCostRecorded:              {Payloads: []reflect.Type{reflect.TypeOf(llm.CostRecordedPayload{})}},
	llm.EventTypeModeDowngraded:            {Payloads: []reflect.Type{reflect.TypeOf(llm.ModeDowngradedPayload{})}},
	llm.EventTypeRetryWithFeedback:         {Payloads: []reflect.Type{reflect.TypeOf(llm.RetryWithFeedbackPayload{})}},
	llm.EventTypePostureReadAdmin:          {Payloads: []reflect.Type{reflect.TypeOf(llm.PostureReadAdminPayload{})}},
	llm.EventTypeCompletionChunk:           {Payloads: []reflect.Type{reflect.TypeOf(llm.CompletionChunkPayload{})}},
	llm.EventTypeProviderFileUploaded:      {Payloads: []reflect.Type{reflect.TypeOf(llm.ProviderFileUploadedPayload{})}},
	llm.EventTypeProviderCredentialFetched: {Payloads: []reflect.Type{reflect.TypeOf(llm.LLMProviderCredentialFetchedPayload{})}},

	// --- Memory.
	memory.EventTypeMemoryIdentityRejected: {Payloads: []reflect.Type{reflect.TypeOf(memory.MemoryIdentityRejectedPayload{})}},
	memory.EventTypeMemoryHealthChanged:    {Payloads: []reflect.Type{reflect.TypeOf(memory.HealthChangedPayload{})}},
	memory.EventTypeMemoryRecoveryDropped:  {Payloads: []reflect.Type{reflect.TypeOf(memory.RecoveryDroppedPayload{})}},
	memory.EventTypeMemoryItemPut:          {Payloads: []reflect.Type{reflect.TypeOf(memory.MemoryMutationPayload{})}},
	memory.EventTypeMemoryItemDeleted:      {Payloads: []reflect.Type{reflect.TypeOf(memory.MemoryMutationPayload{})}},

	memory.EventTypeMemoryCallerBlockAdmitted: {Payloads: []reflect.Type{reflect.TypeOf(memory.CallerBlockAdmittedPayload{})}},

	// --- Planner.
	planner.EventTypePlannerDecision:                 {Payloads: []reflect.Type{reflect.TypeOf(planner.DecisionPayload{})}},
	planner.EventTypePlannerFinish:                   {Note: "Registered anchor — no canonical emit path in this version (a planner's terminal decision is observable via `planner.decision` with `DecisionKind: Finish` and the `task.*` lifecycle events)."},
	planner.EventTypePlannerError:                    {Note: "Registered anchor — no canonical emit path in this version (planner failures surface as `task.failed`)."},
	planner.EventTypePlannerRepairExhausted:          {Payloads: []reflect.Type{reflect.TypeOf(planner.RepairExhaustedPayload{})}},
	planner.EventTypePlannerMaxStepsExceeded:         {Payloads: []reflect.Type{reflect.TypeOf(planner.MaxStepsExceededPayload{})}},
	planner.EventTypeTrajectoryCompressed:            {Payloads: []reflect.Type{reflect.TypeOf(planner.TrajectoryCompressedPayload{})}},
	planner.EventTypeTrajectoryCompressionFailed:     {Payloads: []reflect.Type{reflect.TypeOf(planner.TrajectoryCompressionFailedPayload{})}},
	planner.EventTypePlannerRepairGuidanceInjected:   {Payloads: []reflect.Type{reflect.TypeOf(planner.RepairGuidanceInjectedPayload{})}},
	planner.EventTypePlannerActionExtraFieldDropped:  {Payloads: []reflect.Type{reflect.TypeOf(planner.ActionExtraFieldDroppedPayload{})}},
	planner.EventTypePlannerToolDeclarationCollision: {Payloads: []reflect.Type{reflect.TypeOf(planner.ToolDeclarationCollisionPayload{})}},

	// --- Protocol edge (artifacts surface + auth middleware + Console
	// flows-page telemetry).
	protocol.EventTypeArtifactUploaded: {Payloads: []reflect.Type{reflect.TypeOf(protocol.ArtifactUploadedPayload{})}},
	protocol.EventTypeArtifactDeleted:  {Payloads: []reflect.Type{reflect.TypeOf(protocol.ArtifactDeletedPayload{})}},
	protoauth.EventTypeAuthRejected:    {Payloads: []reflect.Type{reflect.TypeOf(protoauth.AuthRejectedPayload{})}},
	stream.EventTypeFlowsPageViewed:    {Payloads: []reflect.Type{reflect.TypeOf(stream.FlowsPageViewedPayload{})}},
	stream.EventTypeFlowsRunInvoked:    {Payloads: []reflect.Type{reflect.TypeOf(stream.FlowsRunInvokedPayload{})}},

	// --- Engine flow budgets.
	flow.EventTypeFlowBudgetExceeded: {Payloads: []reflect.Type{reflect.TypeOf(flow.BudgetExceededPayload{})}},

	// --- Notifications topic — one shared payload.
	notifications.EventTypeNotificationTaskFailed:               {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationToolApprovalRequested:    {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationGovernanceBudgetExceeded: {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationAuthRequired:             {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationPauseRequested:           {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationTaskGroupResolved:        {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationTaskGroupCancelled:       {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationTaskCompleted:            {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},
	notifications.EventTypeNotificationIdentityRejected:         {Payloads: []reflect.Type{reflect.TypeOf(notifications.NotificationPayload{})}},

	// --- Unified pause/resume primitive.
	pauseresume.EventTypePauseRequested:             {Payloads: []reflect.Type{reflect.TypeOf(pauseresume.PauseRequestedPayload{})}},
	pauseresume.EventTypePauseResumed:               {Payloads: []reflect.Type{reflect.TypeOf(pauseresume.PauseResumedPayload{})}},
	pauseresume.EventTypePausePayloadArtifactRouted: {Payloads: []reflect.Type{reflect.TypeOf(pauseresume.PausePayloadArtifactRoutedPayload{})}},

	// --- Agent Registry.
	registry.EventTypeAgentRegistered:       {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentRegisteredPayload{})}},
	registry.EventTypeAgentRestarted:        {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentRestartedPayload{})}},
	registry.EventTypeAgentHealth:           {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentHealthPayload{})}},
	registry.EventTypeAgentDeregistered:     {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentDeregisteredPayload{})}},
	registry.EventTypeAgentDrained:          {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentControlPayload{})}},
	registry.EventTypeAgentPaused:           {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentControlPayload{})}},
	registry.EventTypeAgentRestartRequested: {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentControlPayload{})}},
	registry.EventTypeAgentForceStopped:     {Payloads: []reflect.Type{reflect.TypeOf(registry.AgentControlPayload{})}},

	// --- Steering control lifecycle.
	steering.EventTypeControlRejected: {Payloads: []reflect.Type{reflect.TypeOf(steering.ControlRejectedPayload{})}},
	steering.EventTypeControlReceived: {Payloads: []reflect.Type{reflect.TypeOf(steering.ControlLifecyclePayload{})}},
	steering.EventTypeControlApplied:  {Payloads: []reflect.Type{reflect.TypeOf(steering.ControlLifecyclePayload{})}},

	// --- Run-completion hook lifecycle.
	steering.EventTypeRunHookDispatched: {Payloads: []reflect.Type{reflect.TypeOf(steering.RunHookDispatchedPayload{})}},
	steering.EventTypeRunHookFailed:     {Payloads: []reflect.Type{reflect.TypeOf(steering.RunHookFailedPayload{})}},

	// --- Session auto-naming.
	steering.EventTypeSessionNamingFailed: {Payloads: []reflect.Type{reflect.TypeOf(steering.SessionNamingFailedPayload{})}},

	// --- Sessions.
	sessions.EventTypeSessionOpened:       {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionOpenedPayload{})}},
	sessions.EventTypeSessionTouched:      {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionTouchedPayload{})}},
	sessions.EventTypeSessionClosed:       {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionClosedPayload{})}},
	sessions.EventTypeSessionGCReaped:     {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionGCReapedPayload{})}},
	sessions.EventTypeSessionReopened:     {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionReopenedPayload{})}},
	sessions.EventTypeSessionErased:       {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionErasedPayload{})}},
	sessions.EventTypeSessionTitleChanged: {Payloads: []reflect.Type{reflect.TypeOf(sessions.SessionTitleChangedPayload{})}},

	// --- Skills.
	skills.EventTypeSkillUpserted:             {Payloads: []reflect.Type{reflect.TypeOf(skills.SkillUpsertedPayload{})}},
	skills.EventTypeSkillDeleted:              {Payloads: []reflect.Type{reflect.TypeOf(skills.SkillDeletedPayload{})}},
	skills.EventTypeSkillPackOverwriteRefused: {Payloads: []reflect.Type{reflect.TypeOf(skills.SkillPackOverwriteRefusedPayload{})}},
	skills.EventTypeSkillSearchExecuted:       {Payloads: []reflect.Type{reflect.TypeOf(skills.SkillSearchExecutedPayload{})}},
	skills.EventTypeSkillIdentityRejected:     {Payloads: []reflect.Type{reflect.TypeOf(skills.SkillIdentityRejectedPayload{})}},
	skills.EventTypeSkillProposed:             {Payloads: []reflect.Type{reflect.TypeOf(generator.SkillProposedPayload{})}},

	// --- Tasks lifecycle.
	tasks.EventTypeTaskSpawned:                {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskSpawnedPayload{})}},
	tasks.EventTypeTaskStarted:                {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskStartedPayload{})}},
	tasks.EventTypeTaskPaused:                 {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskPausedPayload{})}, Note: "Registry-driver scope: emitted by the task registry's MarkPaused transition, which no V1 production caller drives on the live pause path — a paused run stays `running` in the task projection and pause state travels on the `pause.*` events (see the [pause model](./pause-model.md))."},
	tasks.EventTypeTaskResumed:                {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskResumedPayload{})}, Note: "Registry-driver scope: emitted by the task registry's MarkResumed transition, which no V1 production caller drives on the live pause path — subscribe to `pause.resumed` for live resume signals (see the [pause model](./pause-model.md))."},
	tasks.EventTypeTaskCompleted:              {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskCompletedPayload{})}},
	tasks.EventTypeTaskFailed:                 {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskFailedPayload{})}},
	tasks.EventTypeTaskCancelled:              {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskCancelledPayload{})}},
	tasks.EventTypeTaskPrioritised:            {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskPrioritisedPayload{})}},
	tasks.EventTypeTaskGroupCreated:           {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskGroupCreatedPayload{})}},
	tasks.EventTypeTaskGroupSealed:            {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskGroupSealedPayload{})}},
	tasks.EventTypeTaskGroupResolved:          {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskGroupResolvedPayload{})}},
	tasks.EventTypeTaskGroupCancelled:         {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskGroupCancelledPayload{})}},
	tasks.EventTypeTaskPatchApplied:           {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskPatchAppliedPayload{})}},
	tasks.EventTypeTaskPatchRejected:          {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskPatchRejectedPayload{})}},
	tasks.EventTypeTaskBackgroundAcknowledged: {Payloads: []reflect.Type{reflect.TypeOf(tasks.TaskBackgroundAcknowledgedPayload{})}},
	runctx.EventTypeInputDispositionResolved:  {Payloads: []reflect.Type{reflect.TypeOf(runctx.InputDispositionResolvedPayload{})}},

	// --- Tools execution + approval + OAuth + MCP southbound.
	tools.EventTypeToolInvoked:                        {Payloads: []reflect.Type{reflect.TypeOf(tools.ToolInvokedPayload{})}},
	tools.EventTypeToolCompleted:                      {Payloads: []reflect.Type{reflect.TypeOf(tools.ToolCompletedPayload{})}},
	tools.EventTypeToolFailed:                         {Payloads: []reflect.Type{reflect.TypeOf(tools.ToolFailedPayload{})}},
	tools.EventTypeToolInvalidArgs:                    {Payloads: []reflect.Type{reflect.TypeOf(tools.ToolInvalidArgsPayload{})}},
	tools.EventTypeToolPolicyExhausted:                {Payloads: []reflect.Type{reflect.TypeOf(tools.ToolPolicyExhaustedPayload{})}},
	approval.EventTypeToolApprovalRequested:           {Payloads: []reflect.Type{reflect.TypeOf(approval.ToolApprovalRequestedPayload{})}},
	approval.EventTypeToolApproved:                    {Payloads: []reflect.Type{reflect.TypeOf(approval.ToolApprovedPayload{})}},
	approval.EventTypeToolRejected:                    {Payloads: []reflect.Type{reflect.TypeOf(approval.ToolRejectedPayload{})}},
	toolauth.EventTypeToolAuthRequired:                {Payloads: []reflect.Type{reflect.TypeOf(toolauth.ToolAuthRequiredPayload{})}},
	toolauth.EventTypeToolAuthCompleted:               {Payloads: []reflect.Type{reflect.TypeOf(toolauth.ToolAuthCompletedPayload{})}},
	toolauth.EventTypeToolCredentialExchanged:         {Payloads: []reflect.Type{reflect.TypeOf(toolauth.ToolCredentialExchangedPayload{})}},
	credsource.EventTypeProviderCredentialFetched:     {Payloads: []reflect.Type{reflect.TypeOf(credsource.ProviderCredentialFetchedPayload{})}},
	credsource.EventTypeProviderCredentialFetchFailed: {Payloads: []reflect.Type{reflect.TypeOf(credsource.ProviderCredentialFetchFailedPayload{})}},
	mcptool.EventTypeMCPResourceUpdated:               {Payloads: []reflect.Type{reflect.TypeOf(mcptool.ResourceUpdatedPayload{})}},
	mcptool.EventTypeMCPResourceOffloaded:             {Payloads: []reflect.Type{reflect.TypeOf(mcptool.ResourceOffloadedPayload{})}},
	mcptool.EventTypeMCPAppAvailable:                  {Payloads: []reflect.Type{reflect.TypeOf(mcptool.AppAvailablePayload{})}},
	mcptool.EventTypeMCPArtifactEgressed:              {Payloads: []reflect.Type{reflect.TypeOf(mcptool.ArtifactEgressedPayload{})}},
}

// safePayloadIface / eventPayloadIface back the mechanical Safe-vs-
// redacted classification: a payload type that implements
// events.SafePayload reaches subscribers typed; everything else is
// walked by the audit redactor and arrives as a RedactedMap.
var (
	safePayloadIface  = reflect.TypeOf((*events.SafePayload)(nil)).Elem()
	eventPayloadIface = reflect.TypeOf((*events.EventPayload)(nil)).Elem()
)

// renderEventsPage emits events.md: the exhaustive registry-read event
// catalog with field-level payload shapes.
func renderEventsPage() (string, error) {
	var b strings.Builder
	b.WriteString(generatedHeader + "\n\n")
	b.WriteString("# Protocol events\n\n")
	fmt.Fprintf(&b, "The %d canonical event types a Harbor Runtime can publish, read from the live\n", len(events.EventTypes()))
	b.WriteString("event-type registry (`internal/events`) as the production driver set populates it.\n")
	b.WriteString("Subscribe via `GET /v1/events` (SSE) — see [methods.md](./methods.md#streaming-events)\n")
	b.WriteString("and the [streaming semantics guide](./streaming-semantics.md).\n\n")
	b.WriteString("## How payloads reach a subscriber\n\n")
	b.WriteString("Every SSE frame's `data:` line is one JSON object: `type`, `sequence` (the reconnect\n")
	b.WriteString("cursor), `occurred_at`, the flat identity (`tenant` / `user` / `session` / `run`), and\n")
	b.WriteString("`payload`. Two payload classes exist:\n\n")
	b.WriteString("- **Safe payloads** — types the declaring subsystem marked as carrying no secret-shaped\n")
	b.WriteString("  data. The bus skips the audit redactor; the subscriber receives the typed shape below\n")
	b.WriteString("  verbatim. Field keys are whatever `encoding/json` emits: most payloads carry no\n")
	b.WriteString("  struct tags, so their keys are the Go field names (e.g. `TaskID`, capital `T`); a\n")
	b.WriteString("  payload whose fields ARE tagged uses the tag name. The Wire key column below is\n")
	b.WriteString("  authoritative per payload — read it, don't assume the casing.\n")
	b.WriteString("- **Redacted payloads** — everything else is walked by the audit redactor on publish;\n")
	b.WriteString("  the subscriber receives a redacted key/value map derived from the declared shape.\n")

	for _, t := range events.EventTypes() {
		entry, ok := eventPayloadIndex[t]
		if !ok {
			return "", fmt.Errorf("event type %q has no eventPayloadIndex entry — extend the index (the lockstep test pins this)", t)
		}
		fmt.Fprintf(&b, "\n## `%s`\n", t)
		if len(entry.Payloads) == 0 {
			fmt.Fprintf(&b, "\n%s\n", entry.Note)
			continue
		}
		if entry.Note != "" {
			fmt.Fprintf(&b, "\n%s\n", entry.Note)
		}
		for _, pt := range entry.Payloads {
			if !pt.Implements(eventPayloadIface) && !reflect.PointerTo(pt).Implements(eventPayloadIface) {
				return "", fmt.Errorf("event type %q: indexed payload %s does not implement events.EventPayload", t, pt)
			}
			delivery := "redacted on the wire (audit-redactor walk; subscribers receive a redacted map)"
			if pt.Implements(safePayloadIface) || reflect.PointerTo(pt).Implements(safePayloadIface) {
				delivery = "safe payload (delivered typed, verbatim)"
			}
			fmt.Fprintf(&b, "\nPayload `%s` — %s.\n\n", pt.Name(), delivery)
			renderStructFields(&b, pt)
		}
	}

	return b.String(), nil
}
