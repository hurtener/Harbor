package agentcfg

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// agent-config canonical event types. Registered via init() so the
// canonical events registry stays the single source of truth (register at
// declaration time, publish at use time).
//
// Both payloads are SafePayload (compose events.SafeSealed): the agent
// id, revision ids, content hash, and author identity are operator-visible
// audit metadata, never secret-shaped. The payload carries NO config
// content (no skill names, no prompt text) — only the revision identity —
// so the audit trail never leaks the desired-state body (CLAUDE.md §7).
const (
	// EventTypeConfigRevised — emitted when a new agent-config revision is
	// written (any successful SetRevision, including the skills-control
	// consumer). Carries the agent id, the new + parent revision ids, the
	// content hash, and the author identity.
	EventTypeConfigRevised events.EventType = "agent.config.revised"

	// EventTypeConfigReverted — emitted when an admin rolls the active
	// pointer back to an existing revision. Carries the agent id, the
	// revision rolled back TO, the previously-active revision id, and the
	// author identity. Rollback never mutates a revision; the event marks
	// the active-pointer repoint.
	EventTypeConfigReverted events.EventType = "agent.config.reverted"

	// EventTypeMCPConnectionPaused — emitted when a tool-exposure edit newly
	// pauses an MCP server (the server is added to the active revision's
	// paused set). The transport stays warm — pause is projection-time, so
	// the server's tools are excluded from the next run and its App callbacks
	// are rejected against current state. Carries the agent id, the paused
	// server id, the recording revision id, and the author identity — never a
	// secret. Drives the operator-legible "paused by a system administrator"
	// overlay (the client renders the canonical event; it never reaches into
	// the App).
	EventTypeMCPConnectionPaused events.EventType = "mcp.connection.paused"

	// EventTypeMCPConnectionResumed — emitted when a tool-exposure edit newly
	// resumes an MCP server (the server is removed from the active revision's
	// paused set). Resume is an instant flag flip — no re-dial. Carries the
	// same operator-visible audit metadata as the paused event.
	EventTypeMCPConnectionResumed events.EventType = "mcp.connection.resumed"

	// EventTypeMCPConnectionPending — emitted when an admin add of a NEW MCP
	// server connection begins the attach lifecycle (dial → initialize →
	// discover → register). Marks the `pending` lifecycle transition before
	// the terminal added / failed / auth_required event. Carries the agent
	// id, the server name, and the author identity — never a secret.
	EventTypeMCPConnectionPending events.EventType = "mcp.connection.pending"

	// EventTypeMCPConnectionAdded — emitted when an admin add of a NEW MCP
	// server connection completes the attach lifecycle successfully: the
	// transport dialled, the initialize handshake succeeded, discovery ran,
	// and the server's tools were registered on the live catalog. Carries
	// the agent id, the server name, the recording revision id, and the
	// author identity — never a secret header / token / credential.
	EventTypeMCPConnectionAdded events.EventType = "mcp.connection.added"

	// EventTypeMCPConnectionFailed — emitted when an admin add of a NEW MCP
	// server connection FAILS at any attach step (unreachable, handshake /
	// version mismatch, malformed discovery). A half-attached server is NEVER
	// registered (fail loud, no silent drop). Carries the agent id, the
	// server name, and a SAFE failure reason — never a secret. No revision is
	// recorded for a failed add (the broken connection is not desired state).
	EventTypeMCPConnectionFailed events.EventType = "mcp.connection.failed"

	// EventTypeMCPConnectionAuthRequired — emitted when an admin add of a NEW
	// MCP server connection requires authorization: the attach parks on the
	// unified pause/resume primitive (the tool-side OAuth lineage) and a
	// resume completes the attach. Carries the agent id, the server name, the
	// recording revision id, the pause token, and the author identity — never
	// a secret / credential / auth code.
	EventTypeMCPConnectionAuthRequired events.EventType = "mcp.connection.auth_required"

	// EventTypeMCPConnectionRemoved — emitted when an admin removes a
	// runtime-added MCP server connection (agent_config.remove_mcp_connection):
	// a new revision drops the named descriptor and prunes that server's
	// tool-exposure residue. The physical teardown (deregister the server's
	// tools + close its transport) happens at the NEXT run's projection
	// boundary — this event marks the desired-state removal, not the transport
	// close. Carries the agent id, the removed server id, the recording
	// revision id, and the author identity — never a secret.
	EventTypeMCPConnectionRemoved events.EventType = "mcp.connection.removed"

	// EventTypeMCPDiscoveryOriginsSet — emitted when an admin FULL-REPLACES a
	// runtime-added MCP connection's OAuth-discovery cross-origin allow-list
	// (agent_config.set_mcp_discovery_origins). Carries the agent id, the
	// connection id, the recording revision id, and the granted / revoked origin
	// deltas — all NON-SECRET (public https origins are an allow-list, never a
	// credential). This is the admin-scope audit event the write emits as one
	// fail-closed unit with the mutation (CLAUDE.md §7).
	EventTypeMCPDiscoveryOriginsSet events.EventType = "mcp.connection.discovery_origins_set"
)

func init() {
	for _, t := range []events.EventType{
		EventTypeConfigRevised,
		EventTypeConfigReverted,
		EventTypeMCPConnectionPaused,
		EventTypeMCPConnectionResumed,
		EventTypeMCPConnectionPending,
		EventTypeMCPConnectionAdded,
		EventTypeMCPConnectionFailed,
		EventTypeMCPConnectionAuthRequired,
		EventTypeMCPConnectionRemoved,
		EventTypeMCPDiscoveryOriginsSet,
	} {
		events.RegisterEventType(t)
	}
}

// ConfigRevisedPayload is the typed payload for EventTypeConfigRevised.
// SafePayload — every field is operator-visible audit metadata; no config
// content is carried.
type ConfigRevisedPayload struct {
	events.SafeSealed
	// Author is the identity that wrote the revision.
	Author identity.Quadruple
	// AgentID is the agent whose config was revised (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// RevisionID is the new revision's id.
	RevisionID string
	// ParentRevisionID is the revision the new one descends from (empty
	// for the first revision).
	ParentRevisionID string
	// ContentHash is the new revision's content hash (for diff/audit
	// correlation; never the content itself).
	ContentHash string
	// OccurredAt is the revision's creation instant.
	OccurredAt time.Time
}

// ConfigRevertedPayload is the typed payload for EventTypeConfigReverted.
// SafePayload — operator-visible audit metadata only.
type ConfigRevertedPayload struct {
	events.SafeSealed
	// Author is the identity that performed the rollback.
	Author identity.Quadruple
	// AgentID is the agent whose active pointer was repointed.
	AgentID string
	// RevisionID is the revision the active pointer now points to.
	RevisionID string
	// FromRevisionID is the previously-active revision id (empty when the
	// agent had no active pointer before the rollback).
	FromRevisionID string
	// OccurredAt is the rollback instant.
	OccurredAt time.Time
}

// MCPConnectionPausedPayload is the typed payload for
// EventTypeMCPConnectionPaused. SafePayload — every field is
// operator-visible audit metadata; no secret-shaped content is carried.
type MCPConnectionPausedPayload struct {
	events.SafeSealed
	// Author is the identity that recorded the pause.
	Author identity.Quadruple
	// AgentID is the agent whose MCP exposure changed (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ServerID is the MCP source id newly paused.
	ServerID string
	// RevisionID is the tool-exposure revision that recorded the pause.
	RevisionID string
	// OccurredAt is the pause instant.
	OccurredAt time.Time
}

// MCPConnectionResumedPayload is the typed payload for
// EventTypeMCPConnectionResumed. SafePayload — operator-visible audit
// metadata only.
type MCPConnectionResumedPayload struct {
	events.SafeSealed
	// Author is the identity that recorded the resume.
	Author identity.Quadruple
	// AgentID is the agent whose MCP exposure changed.
	AgentID string
	// ServerID is the MCP source id newly resumed.
	ServerID string
	// RevisionID is the tool-exposure revision that recorded the resume.
	RevisionID string
	// OccurredAt is the resume instant.
	OccurredAt time.Time
}

// MCPConnectionRemovedPayload is the typed payload for
// EventTypeMCPConnectionRemoved. SafePayload — every field is
// operator-visible audit metadata; no secret-shaped content is carried. It
// mirrors the paused/resumed payload shape (the removal is the same class of
// desired-state edit).
type MCPConnectionRemovedPayload struct {
	events.SafeSealed
	// Author is the identity that recorded the removal.
	Author identity.Quadruple
	// AgentID is the agent whose MCP connection was removed (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ServerID is the MCP source id that was removed.
	ServerID string
	// RevisionID is the revision that dropped the descriptor + pruned residue.
	RevisionID string
	// OccurredAt is the removal instant.
	OccurredAt time.Time
}

// MCPDiscoveryOriginsSetPayload is the typed payload for
// EventTypeMCPDiscoveryOriginsSet. SafePayload — every field is
// operator-visible audit metadata; the origin deltas are PUBLIC https origins
// (an allow-list), never a secret.
type MCPDiscoveryOriginsSetPayload struct {
	events.SafeSealed
	// Author is the identity that performed the write.
	Author identity.Quadruple
	// AgentID is the agent whose connection allow-list changed (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ServerID is the MCP source id whose allow-list was replaced.
	ServerID string
	// RevisionID is the revision that recorded the new allow-list.
	RevisionID string
	// Granted is the set of origins newly present versus the prior live set.
	Granted []string
	// Revoked is the set of origins dropped versus the prior live set.
	Revoked []string
	// OccurredAt is the write instant.
	OccurredAt time.Time
}

// MCPConnectionLifecyclePayload is the typed payload for the runtime
// MCP-attach lifecycle events (pending / added / failed / auth_required).
// SafePayload — every field is operator-visible audit metadata; NO secret
// header / token / credential / auth code is ever carried (CLAUDE.md §7).
type MCPConnectionLifecyclePayload struct {
	events.SafeSealed
	// Author is the identity that drove the add.
	Author identity.Quadruple
	// AgentID is the agent the connection was added to (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ServerID is the MCP source id (name) of the connection being added.
	ServerID string
	// Transport is the connection transport ("stdio" / "http") — non-secret.
	Transport string
	// State is the lifecycle state the event marks ("pending" / "online" /
	// "failed" / "auth_required").
	State string
	// RevisionID is the recording revision id (set for online / auth_required;
	// empty for pending and for a failed add, which records no revision).
	RevisionID string
	// PauseToken is the unified-pause/resume token the auth_required attach
	// parked on (set only for the auth_required state); empty otherwise. It
	// is an opaque runtime handle, NOT a credential.
	PauseToken string
	// Reason is a SAFE, operator-facing failure reason (set only for the
	// failed state); never a secret or raw transport payload.
	Reason string
	// OccurredAt is the lifecycle-transition instant.
	OccurredAt time.Time
}
