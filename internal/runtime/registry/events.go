package registry

import (
	"github.com/hurtener/Harbor/internal/events"
)

// Agent lifecycle + fleet-control event types. Each is registered with
// the events package's exhaustive registry via init() so Publish
// accepts them without ErrUnknownEventType. Subscribers (the Console
// Agents page lens, later phases) filter on these via events.Filter.Types.
//
// Every agent.* event carries the registration agent_id in its payload
// (RFC §6.16 "Events").
const (
	// EventTypeAgentRegistered — a NEW logical agent was registered
	// (incarnation 1). Payload: AgentRegisteredPayload.
	EventTypeAgentRegistered events.EventType = "agent.registered"
	// EventTypeAgentRestarted — a KNOWN logical agent was re-registered
	// (incarnation bumped; restart != recreate). Payload:
	// AgentRestartedPayload.
	EventTypeAgentRestarted events.EventType = "agent.restarted"
	// EventTypeAgentHealth — an agent's Health was reported / changed.
	// Payload: AgentHealthPayload.
	EventTypeAgentHealth events.EventType = "agent.health"
	// EventTypeAgentDrained — a Drain fleet-control command was issued.
	// Payload: AgentControlPayload.
	EventTypeAgentDrained events.EventType = "agent.drained"
	// EventTypeAgentDeregistered — an agent's record was removed.
	// Payload: AgentDeregisteredPayload.
	EventTypeAgentDeregistered events.EventType = "agent.deregistered"
	// EventTypeAgentPaused — a Pause fleet-control command was issued.
	// Payload: AgentControlPayload.
	EventTypeAgentPaused events.EventType = "agent.paused"
	// EventTypeAgentRestartRequested — a Restart fleet-control command
	// was issued. Distinct from agent.restarted (which is the registry
	// observing a re-registration); this is the operator REQUESTING a
	// restart. Payload: AgentControlPayload.
	EventTypeAgentRestartRequested events.EventType = "agent.restart_requested"
	// EventTypeAgentForceStopped — a ForceStop fleet-control command
	// was issued. Payload: AgentControlPayload.
	EventTypeAgentForceStopped events.EventType = "agent.force_stopped"
)

func init() {
	events.RegisterEventType(EventTypeAgentRegistered)
	events.RegisterEventType(EventTypeAgentRestarted)
	events.RegisterEventType(EventTypeAgentHealth)
	events.RegisterEventType(EventTypeAgentDrained)
	events.RegisterEventType(EventTypeAgentDeregistered)
	events.RegisterEventType(EventTypeAgentPaused)
	events.RegisterEventType(EventTypeAgentRestartRequested)
	events.RegisterEventType(EventTypeAgentForceStopped)
}

// AgentRegisteredPayload reports a first registration (incarnation 1).
// Carries the registration agent_id; the identity triple lives on the
// Event itself, so it is intentionally NOT duplicated here.
//
// SafePayload by construction — no secret-shaped fields. DisplayName /
// RegistrationKey are operator-controlled cosmetic labels, not
// secret-shaped material.
type AgentRegisteredPayload struct {
	events.SafeSealed
	AgentID         string
	RegistrationKey string
	VersionHash     string
	Hosting         string
	Incarnation     uint64
	RegisteredAt    int64
}

// AgentRestartedPayload reports a re-registration of a known agent
// (incarnation bumped). VersionHashChanged distinguishes "restarted, no
// change" from "restarted after a config edit" — the Console renders
// these differently. SafePayload by construction.
type AgentRestartedPayload struct {
	events.SafeSealed
	AgentID            string
	RegistrationKey    string
	VersionHash        string
	Incarnation        uint64
	RestartedAt        int64
	VersionHashChanged bool
}

// AgentHealthPayload reports a Health report / change. SafePayload by
// construction — Health is a closed enum.
type AgentHealthPayload struct {
	events.SafeSealed
	AgentID    string
	Health     string // string form of Health
	ReportedAt int64  // unix nanoseconds
}

// AgentDeregisteredPayload reports an agent record removal. SafePayload
// by construction.
type AgentDeregisteredPayload struct {
	events.SafeSealed
	AgentID         string
	RegistrationKey string
	DeregisteredAt  int64 // unix nanoseconds
}

// AgentControlPayload reports a fleet-control command (Pause / Drain /
// Restart / ForceStop). Command is one of "pause" / "drain" /
// "restart" / "force_stop". Reason is the operator-supplied,
// audit-redacted reason string — callers MUST NOT pass tool args, raw
// user input, or secret-shaped material; the registry runs Reason
// through the audit.Redactor before this payload is built, and the
// bus does not re-redact SafePayload types (D-020 / D-028).
//
// SafePayload by construction: every field is either a closed enum, an
// id, a timestamp, or an already-redacted string.
type AgentControlPayload struct {
	events.SafeSealed
	AgentID  string
	Command  string
	Reason   string // already passed through audit.Redactor
	IssuedAt int64  // unix nanoseconds
}
