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

	// EventTypeOAuthProviderInstalled — emitted when an admin installs (upserts)
	// a Protocol-installed OAuth provider (agent_config.set_oauth_provider): a
	// new revision records the ZERO-URL descriptor AND the provider is installed
	// live into the owner-tagged provider set. Carries the agent id, the provider
	// name, the credential-broker name, the recording revision id, and the author
	// identity — all NON-SECRET. NO URL, NO env-var name, NO secret is ever
	// carried (there is none on the descriptor). Emitted as one fail-closed unit
	// with the mutation (CLAUDE.md §7).
	EventTypeOAuthProviderInstalled events.EventType = "agent_config.oauth_provider.installed"

	// EventTypeOAuthProviderRemoved — emitted when an admin uninstalls a
	// Protocol-installed OAuth provider (agent_config.remove_oauth_provider): a
	// new revision drops the descriptor AND the provider is uninstalled live
	// (CLOSED). Carries the agent id, the provider name, the recording revision
	// id, and the author identity — all NON-SECRET. Emitted as one fail-closed
	// unit with the mutation.
	EventTypeOAuthProviderRemoved events.EventType = "agent_config.oauth_provider.removed"

	// EventTypeMCPConnectionReattached — emitted when the RUN-START attach leg
	// re-establishes a runtime-added MCP connection the reconciling owner's
	// active config revision DECLARES but the live registry did not carry (a
	// fresh process on the same state store, or a rollback that re-declares a
	// previously-removed connection). It is deliberately NOT
	// EventTypeMCPConnectionAdded: that type's godoc binds it to an admin add,
	// and a reconcile has no admin request. A consumer also distinguishes the
	// two families structurally — an admin add's Author quadruple carries an
	// EMPTY RunID, a reconcile's carries the reconciling run's. Payload:
	// MCPConnectionLifecyclePayload with State "online"; never a secret.
	EventTypeMCPConnectionReattached events.EventType = "mcp.connection.reattached"

	// EventTypeMCPConnectionReattachFailed — emitted when the run-start attach
	// leg could NOT re-establish a DECLARED connection. The run is never failed
	// by it and the sweep is never aborted, but the shortfall is never silent
	// either: the outcome is REPORTED with a stable class so an operator surface
	// can tell "unreachable third party" from "refused by current boot policy".
	// Payload: MCPConnectionLifecyclePayload, where State carries the stable
	// class (one of the MCPReattachClass* constants) and Reason carries the
	// scrubbed, length-bounded operator-facing reason. Never a secret.
	EventTypeMCPConnectionReattachFailed events.EventType = "mcp.connection.reattach_failed"

	// EventTypeLLMProviderInstalled — emitted when an admin installs (upserts)
	// / rotates a Protocol-installed, ZERO-URL inference provider binding
	// (agent_config.set_llm_provider): the binding is installed live into the
	// owner-tagged provider set so the runtime's LLM key is sourced from the
	// named coordinator broker. Carries the agent id, the binding name, the
	// provider, the inference-broker name, and the author identity — all
	// NON-SECRET. NO URL, NO env-var name, NO secret is ever carried (there is
	// none on the descriptor). Emitted as one fail-closed unit with the
	// mutation (CLAUDE.md §7).
	EventTypeLLMProviderInstalled events.EventType = "agent_config.llm_provider.installed"
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
		EventTypeMCPConnectionReattached,
		EventTypeMCPConnectionReattachFailed,
		EventTypeMCPDiscoveryOriginsSet,
		EventTypeOAuthProviderInstalled,
		EventTypeOAuthProviderRemoved,
		EventTypeLLMProviderInstalled,
	} {
		events.RegisterEventType(t)
	}
}

// The CLOSED set of run-start re-attach failure classes carried in
// MCPConnectionLifecyclePayload.State on EventTypeMCPConnectionReattachFailed.
// They live here, next to the event type, because they are part of the
// observable contract an operator surface reads — not an implementation detail
// of the runtime glue that emits them. Every class is non-secret and stable;
// adding one is an additive contract change, renaming one is breaking.
//
// The split that matters to an operator: MCPReattachClassTransportFailed means
// "the third party did not answer" (retryable — the leg re-dials on a bounded
// backoff), every other class means "current boot policy or a live-registry
// collision refuses this descriptor" (NOT retryable by re-dialling — it is
// emitted once per attempted descriptor and only an operator action clears it).
const (
	// MCPReattachClassTransportFailed — the dial, the initialize handshake, or
	// discovery failed. This is also the class a connection whose live transport
	// depended on operator-supplied static auth headers lands in: those headers
	// are secret and never persisted, so the re-attach dials without them and a
	// server that required them answers 401. RETRYABLE.
	MCPReattachClassTransportFailed = "transport_failed"
	// MCPReattachClassStdioNotAllowed — the declared stdio descriptor's argv[0]
	// is absent from the CURRENT boot stdio allowlist. The revision was written
	// while the command was allowlisted; boot policy has since tightened (or the
	// allowlist is empty, which refuses every stdio re-attach). Never spawned.
	MCPReattachClassStdioNotAllowed = "stdio_not_allowed"
	// MCPReattachClassInjectionDisabled — the declared descriptor carries a
	// per-user credential-injection mapping but the fail-closed
	// `tools.allow_wire_injection` opt-in is OFF. The dev opt-in is a
	// kill-switch: a mapping persisted while it was on is not rebuilt after a
	// restart with it off.
	MCPReattachClassInjectionDisabled = "injection_disabled"
	// MCPReattachClassOAuthBinding — the descriptor's `oauth_provider` (or
	// injection broker) NAME does not resolve, or the connection host is absent
	// from the provider's boot-declared downstream-sink allow-list. A NAME
	// resolution and an allow-list check only — no token participates.
	MCPReattachClassOAuthBinding = "oauth_binding"
	// MCPReattachClassOwnerConflict — the declared name is already live in the
	// registry under a DIFFERENT (tenant, agent) owner. The re-attach is refused
	// rather than evicting another owner's transport and tools. Only that owner's
	// detach or a rename clears it.
	MCPReattachClassOwnerConflict = "owner_conflict"
	// MCPReattachClassAmbiguousServerID — the declared name would make an
	// already-registered server's `<name>_<tool>` ids ambiguous. A naming
	// collision an operator must resolve.
	MCPReattachClassAmbiguousServerID = "ambiguous_server_id"
)

// MCPReattachFailureClasses is the closed class set as a slice, so a consumer
// (and the phase's report guard) can enumerate it rather than re-listing it.
func MCPReattachFailureClasses() []string {
	return []string{
		MCPReattachClassTransportFailed,
		MCPReattachClassStdioNotAllowed,
		MCPReattachClassInjectionDisabled,
		MCPReattachClassOAuthBinding,
		MCPReattachClassOwnerConflict,
		MCPReattachClassAmbiguousServerID,
	}
}

// LLMProviderSetPayload is the typed payload for the Protocol-installed
// inference-provider install / rotate audit event. SafePayload — every field
// is operator-visible audit metadata; NO URL, NO env-var name, NO secret is
// ever carried (there is none on the zero-URL descriptor). The
// inference-broker name is a non-secret boot reference.
type LLMProviderSetPayload struct {
	events.SafeSealed
	// Author is the identity that performed the install / rotate.
	Author identity.Quadruple
	// AgentID is the agent whose installed inference-provider binding changed
	// (a registration identity, NOT an isolation principal).
	AgentID string
	// ProviderName is the installed binding name.
	ProviderName string
	// Provider is the LLM provider the pulled key authenticates.
	Provider string
	// InferenceBroker is the boot-declared broker the binding references by
	// name. NON-SECRET.
	InferenceBroker string
	// OccurredAt is the write instant.
	OccurredAt time.Time
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

// OAuthProviderSetPayload is the typed payload for the Protocol-installed
// OAuth-provider install / uninstall audit events. SafePayload — every field is
// operator-visible audit metadata; NO URL, NO env-var name, NO secret is ever
// carried (there is none on the zero-URL descriptor). The credential-broker name
// is a non-secret boot reference.
type OAuthProviderSetPayload struct {
	events.SafeSealed
	// Author is the identity that performed the install / uninstall.
	Author identity.Quadruple
	// AgentID is the agent whose installed-provider set changed (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ProviderName is the installed / uninstalled provider name.
	ProviderName string
	// CredentialBroker is the boot-declared broker the installed provider
	// references by name (empty on uninstall). NON-SECRET.
	CredentialBroker string
	// RevisionID is the recording revision id.
	RevisionID string
	// OccurredAt is the write instant.
	OccurredAt time.Time
}

// MCPConnectionLifecyclePayload is the typed payload for the runtime
// MCP-attach lifecycle events (pending / added / failed / auth_required) AND
// for the run-start re-attach leg's two events (reattached / reattach_failed),
// which reuse this shape verbatim rather than adding a payload type that would
// differ from it only by a doc comment. SafePayload — every field is
// operator-visible audit metadata; NO secret header / token / credential / auth
// code is ever carried (CLAUDE.md §7).
type MCPConnectionLifecyclePayload struct {
	events.SafeSealed
	// Author is the identity that drove the add. On a run-start re-attach there
	// is no admin request: Author carries the RECONCILING RUN's quadruple, whose
	// non-empty RunID is the machine-readable discriminator between the two
	// families (an admin add's RunID is empty).
	Author identity.Quadruple
	// AgentID is the agent the connection was added to (a registration
	// identity, NOT an isolation principal).
	AgentID string
	// ServerID is the MCP source id (name) of the connection being added.
	ServerID string
	// Transport is the connection transport ("stdio" / "http") — non-secret.
	Transport string
	// State is the lifecycle state the event marks ("pending" / "online" /
	// "failed" / "auth_required"). On the run-start re-attach events it is
	// "online" for a successful re-establishment and the stable failure CLASS
	// (one of the MCPReattachClass* constants) for a refused / unreachable one —
	// so a reader distinguishes "the third party is down" from "current boot
	// policy refuses this descriptor" without parsing Reason.
	State string
	// RevisionID is the recording revision id (set for online / auth_required;
	// empty for pending and for a failed add, which records no revision).
	RevisionID string
	// PauseToken is the unified-pause/resume token the auth_required attach
	// parked on (set only for the auth_required state); empty otherwise. It
	// is an opaque runtime handle, NOT a credential.
	PauseToken string
	// Reason is a SAFE, operator-facing failure reason (set only for the
	// failed state); never a secret or raw transport payload. On a re-attach
	// failure it may additionally carry a suppressed-attempt count, so a
	// backed-off retry window is bounded and reported rather than silent.
	Reason string
	// OccurredAt is the lifecycle-transition instant.
	OccurredAt time.Time
}
