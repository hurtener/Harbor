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
)

func init() {
	for _, t := range []events.EventType{
		EventTypeConfigRevised,
		EventTypeConfigReverted,
		EventTypeMCPConnectionPaused,
		EventTypeMCPConnectionResumed,
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
