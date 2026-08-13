package main

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
)

// canonicalEventNameOracle is deliberately hand-maintained. It is an external
// compatibility oracle for the runtime registry: it must be reviewed whenever
// an event is added, removed, or renamed, rather than being regenerated from
// the emitter constants it verifies.
const canonicalEventNameOracle = `agent.config.reverted
agent.config.revised
agent.deregistered
agent.drained
agent.force_stopped
agent.health
agent.paused
agent.registered
agent.restart_requested
agent.restarted
agent_config.llm_provider.installed
agent_config.oauth_provider.installed
agent_config.oauth_provider.removed
agent_config.retirement.completed
agent_config.retirement.progress
agent_config.retirement.started
artifacts.deleted
artifacts.uploaded
audit.admin_scope_used
audit.redaction_failed
auth.rejected
bus.dropped
bus.subscription_idle_closed
control.applied
control.received
control.rejected
dev.draft.created
dev.draft.discarded
dev.draft.previewed
dev.draft.saved
dev.draft.updated
distributed.bus_envelope
flow.budget_exceeded
flows.page_viewed
flows.run_invoked
governance.budget_exceeded
governance.failover
governance.key_rotated
governance.maxtokens_exceeded
governance.posture_read_admin
governance.posture_set
governance.rate_limited
governance.tenant_overrides_set
llm.completion.chunk
llm.context_leak
llm.context_window_exceeded
llm.cost.recorded
llm.image.materialized
llm.mode_downgraded
llm.posture_read_admin
llm.provider_credential_fetched
llm.provider_file.uploaded
llm.retry_with_feedback
mcp.app_available
mcp.artifact_egressed
mcp.connection.added
mcp.connection.auth_required
mcp.connection.discovery_origins_set
mcp.connection.failed
mcp.connection.paused
mcp.connection.pending
mcp.connection.reattach_failed
mcp.connection.reattached
mcp.connection.removed
mcp.connection.resumed
mcp.raw_html_trust_toggled
mcp.resource_offloaded
mcp.resource_updated
memory.caller_block_admitted
memory.health_changed
memory.identity_rejected
memory.item_deleted
memory.item_put
memory.recovery_dropped
notification.auth_required
notification.governance_budget_exceeded
notification.identity_rejected
notification.pause_requested
notification.task_completed
notification.task_failed
notification.task_group_cancelled
notification.task_group_resolved
notification.tool_approval_requested
pause.payload_artifact_routed
pause.requested
pause.resumed
planner.action_extra_field_dropped
planner.decision
planner.error
planner.finish
planner.max_steps_exceeded
planner.repair_exhausted
planner.repair_guidance_injected
planner.tool_declaration_collision
run.hook_dispatched
run.hook_failed
runs.overrides_set
runtime.error
runtime.run_cancelled
runtime.warning
session.closed
session.erased
session.gc_reaped
session.naming_failed
session.opened
session.reopened
session.title_changed
session.touched
skill.deleted
skill.identity_rejected
skill.pack_overwrite_refused
skill.proposed
skill.search_executed
skill.upserted
task.background_acknowledged
task.cancelled
task.completed
task.failed
task.group_cancelled
task.group_created
task.group_resolved
task.group_sealed
task.input_disposition.resolved
task.patch_applied
task.patch_rejected
task.paused
task.prioritised
task.progress
task.resumed
task.spawned
task.started
tool.approval_requested
tool.approved
tool.auth_completed
tool.auth_required
tool.completed
tool.credential_exchanged
tool.failed
tool.invalid_args
tool.invoked
tool.policy_exhausted
tool.provider_credential_fetch_failed
tool.provider_credential_fetched
tool.rejected
topology.changed
trajectory.compressed
trajectory.compression_failed`

func TestCanonicalEventNameOracle(t *testing.T) {
	want := strings.Fields(canonicalEventNameOracle)
	gotTypes := events.EventTypes()
	got := make([]string, len(gotTypes))
	for i, typ := range gotTypes {
		got[i] = string(typ)
	}
	if len(got) != len(want) {
		t.Fatalf("canonical event-name population=%d, oracle=%d; update the reviewed compatibility oracle deliberately", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonical event-name mismatch at %d: registry=%q oracle=%q; additions, removals, and renames require an explicit oracle update", i, got[i], want[i])
		}
	}
}
