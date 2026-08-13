// Package mcp is Harbor's Model Context Protocol (MCP) southbound
// driver. It implements `tools.ToolProvider` against a remote MCP
// server, exposing the server's tools / resources / prompts as
// Harbor `Tool` entries (RFC §6.4). Three wire transports are
// supported (stdio, SSE, streamable-HTTP) with auto-detect.
//
// Concurrent reuse: a constructed *Provider is safe to share
// across N concurrent goroutines after Connect returns. All per-call
// state lives on the goroutine stack + the request `ctx`; descriptor
// fields are immutable after Discover.
//
// Identity (RFC §4): the (tenant, user, session) triple is forwarded
// to the remote MCP server in the request's `_meta` map so trust
// signals flow across the seam.
//
// Reliability shell: every Invoke runs inside
// `tools.RunWithPolicy` so timeout / retry / classifier behaviour is
// identical to the in-process driver.
package mcp

import (
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
)

// EventTypeMCPResourceUpdated is the canonical event type emitted
// when the remote MCP server pushes a resource-update notification
// for a URI the driver previously subscribed to. The
// payload is SafePayload by construction — the URI is operator-
// trust-equivalent (it originates from an operator-configured MCP
// server) and the source ID is operator-supplied.
const EventTypeMCPResourceUpdated events.EventType = "mcp.resource_updated"

// EventTypeMCPResourceOffloaded is the canonical event the runtime
// emits when an MCP resource read (an MCP App `ui://` document fetch, or
// an app-tool-call result) meets the heavy-content threshold and is
// routed through the ArtifactStore by reference instead of inlined. It
// is the loud bypass record the context-window safety net requires —
// heavy content is never inlined past the threshold and never silently
// truncated; the offload is observable. The payload is SafePayload by
// construction: it carries the artifact reference id, the resource URI
// or tool name, the byte size, and the actor identity quadruple — no
// upstream MCP content bytes.
const EventTypeMCPResourceOffloaded events.EventType = "mcp.resource_offloaded"

// EventTypeMCPAppAvailable is the canonical event the runtime emits when
// an invoked MCP tool declares an interactive MCP App via the
// `_meta.ui.resourceUri` slot on its tool DEFINITION (the spec-conformant
// placement in the `io.modelcontextprotocol/ui` dialect, captured at
// discovery). It is the discovery signal a Protocol client consumes off
// the event stream to mount the inline MCP App renderer in the chat
// surface for the run's turn — without it, a planner-initiated call to a
// tool carrying a `ui://` app reference reaches no surface and the
// renderer never activates. The payload is SafePayload by construction:
// it carries the server source id, the `ui://` resource URI, the
// display-mode hint (empty unless the server supplied one — the renderer
// defaults to inline), the default-deny raw-HTML trust posture, and the
// actor identity quadruple — no upstream MCP content bytes and no
// caller-controlled argument data.
const EventTypeMCPAppAvailable events.EventType = "mcp.app_available"

// EventTypeMCPArtifactEgressed is the canonical event the runtime emits
// when it resolves an artifact reference and places the resolved BYTES
// into an outbound MCP tool call — egress substitution.
//
// It records the FACT of the substitution and nothing else: the
// artifact id, the server, the tool, the parameter, the byte count and
// a `sha256:` digest. Never the bytes.
//
// It exists because dispatch-local byte movement would otherwise be the
// one content-movement path in Harbor that leaves no trace. The nearest
// thing an operator can do without this feature is instruct a model to
// paste content into a tool argument, which is bounded by the LLM
// edge's leak check and leaves a trajectory trail; egress substitution
// is MiB-scale and never enters the model's context, so it would leave
// none. The record closes that gap, and it is emitted FAIL-CLOSED
// before the wire request: a substitution that could not be recorded
// does not happen.
//
// The payload is SafePayload by construction — ids, names, a size and a
// digest identify WHICH bytes moved without carrying them.
const EventTypeMCPArtifactEgressed events.EventType = "mcp.artifact_egressed"

func init() {
	events.RegisterEventType(EventTypeMCPResourceUpdated)
	events.RegisterEventType(EventTypeMCPResourceOffloaded)
	events.RegisterEventType(EventTypeMCPAppAvailable)
	events.RegisterEventType(EventTypeMCPArtifactEgressed)
}

// ErrArtifactEgressUnrecorded — a substitution could not be recorded,
// so it did not happen. Returned when no bus is wired, when the call
// context carries no identity, or when the publish itself failed.
//
// Callers compare with errors.Is. It is a REFUSAL rather than a
// degraded path on purpose: the substitution record is the compensating
// control that makes byte-eligibility acceptable, and a byte movement
// that outlives its own record is exactly what the control exists to
// prevent.
var ErrArtifactEgressUnrecorded = errors.New("mcp: artifact egress substitution could not be recorded; the call is refused rather than moving bytes untraceably")

// ErrArtifactEgressSchema — an artifact-parameter mapping does not
// match the server's OWN discovered inputSchema: it names a tool the
// server does not declare, a parameter that tool does not declare, or a
// parameter the server declares as a non-string type.
//
// It fails the ATTACH rather than the first call, so a server that
// changes its schema out from under a validated mapping is caught at
// the next attach loudly instead of at the next call silently. Callers
// compare with errors.Is.
var ErrArtifactEgressSchema = tools.ErrArtifactEgressSchema

// ErrArtifactEgressNotEligible — a connection carries an
// artifact-parameter mapping without the operator's byte-eligibility
// declaration, or carries either on a transport that cannot deliver
// them. Callers compare with errors.Is.
var ErrArtifactEgressNotEligible = errors.New("mcp: invalid artifact egress declaration")

// ArtifactEgressedPayload is the typed payload for
// EventTypeMCPArtifactEgressed. SafePayload: no artifact content
// survives on the payload — only the actor identity quadruple, the
// server source id, the tool name, and one content-free record per
// substituted parameter.
type ArtifactEgressedPayload struct {
	events.SafeSealed
	// Identity scopes the substitution to the (tenant, user, session)
	// triple the call ran under; its RunID correlates it to the turn.
	// The reachable artifact set was this triple's own and nothing wider.
	Identity identity.Quadruple
	// ServerID is the MCP server the bytes were sent to — the RECIPIENT,
	// which is what byte-eligibility governs and what this record makes
	// auditable.
	ServerID tools.ToolSourceID
	// ToolName is the server-side tool name that received the
	// substitution.
	ToolName string
	// Records is one entry per substituted parameter: the artifact id,
	// the parameter name, the byte count, and the `sha256:` digest that
	// says WHICH bytes moved without carrying them.
	Records []artifactegress.Record
	// OccurredAt is the wall-clock instant the substitution was made —
	// necessarily BEFORE the wire request, because the record is emitted
	// fail-closed ahead of it.
	OccurredAt time.Time
}

// AppAvailablePayload is the typed payload for EventTypeMCPAppAvailable.
// SafePayload: no caller-controlled MCP content survives on the payload —
// only the effective agent id, server source id, the `ui://` resource URI,
// the per-result display-mode hint, the default-deny raw-HTML trust posture,
// and the actor identity quadruple (its RunID correlates the discovery to the
// turn that produced it).
type AppAvailablePayload struct {
	events.SafeSealed
	// Identity scopes the discovery to the (tenant, user, session) triple
	// the tool ran under; its RunID is the turn-correlation key.
	Identity identity.Quadruple
	// AgentID is the reach-admitted effective agent configuration that
	// produced the app. It is server-derived execution authority which clients
	// echo on Apps data-plane calls; it is never caller or MCP content.
	AgentID string
	// Binding is the opaque runtime-issued callback capability for this render.
	Binding string
	// ServerID is the MCP server (source id) hosting the app — the value a
	// client passes to mcp.servers.read_resource to fetch the document.
	ServerID tools.ToolSourceID
	// ToolCallID is the stable per-invocation id (a content hash, not a
	// counter) the client passes to mcp.apps.tool_context to fetch the tool
	// context — the input + lowered result — that produced this app. Safe
	// by construction: an opaque hash, never caller content. EMPTY when no
	// context was captured for the invocation (no capturer wired, or the
	// capture failed): a non-empty id promises a fetchable record, so a
	// client may treat a miss as an expired one and say so.
	ToolCallID string
	// ToolName is the server-side tool name that declared the app — display
	// metadata only. Safe by construction: a tool name, never content.
	ToolName string
	// ResourceURI is the `ui://`-scheme URI of the app's UI document.
	ResourceURI string
	// DisplayMode is the display-mode hint (one of inline / fullscreen /
	// pip), or empty when the server stated none. The tool-definition
	// binding carries no mode in the canonical dialect, so this is empty on
	// the golden path and the renderer defaults to inline; a server MAY
	// supply a per-result hint that wins over the binding.
	DisplayMode string
	// RawHTMLTrusted is the raw-HTML trust posture carried on the
	// discovery. The driver emits the default-deny posture; a client
	// reconciles the full per-server trust via mcp.servers.get.
	RawHTMLTrusted bool
	// OccurredAt is the wall-clock instant the app was discovered.
	OccurredAt time.Time
}

// ResourceOffloadedPayload is the typed payload for
// EventTypeMCPResourceOffloaded. SafePayload: no caller-controlled MCP
// content survives on the payload — only the reference id, the source
// identifier (resource URI or tool name), the byte size, and the actor
// identity quadruple.
type ResourceOffloadedPayload struct {
	events.SafeSealed
	// Identity scopes the offload to the (tenant, user, session) triple
	// the read ran under.
	Identity identity.Quadruple
	// ArtifactID is the content-addressed reference the heavy content
	// was stored under.
	ArtifactID string
	// Source identifies what was offloaded: the resource URI for a
	// `read_resource`, or the tool name for an app-tool-call result.
	Source string
	// SizeBytes is the length of the offloaded content.
	SizeBytes int64
	// OccurredAt is the wall-clock instant the offload happened.
	OccurredAt time.Time
}

// ResourceUpdatedPayload is the typed payload for
// EventTypeMCPResourceUpdated. SafePayload: no caller-controlled
// bytes survive on the payload.
//
//   - Identity scopes the event to the (tenant, user, session)
//     triple under which the resource subscription was registered.
//   - Source is the originating MCP attachment's source ID, so
//     subscribers can route by provider.
//   - URI is the resource URI the server reported as updated; this
//     may be a sub-resource of the URI the client actually
//     subscribed to.
//   - OccurredAt is the wall-clock time the driver received the
//     notification.
type ResourceUpdatedPayload struct {
	events.SafeSealed
	Identity   identity.Quadruple
	Source     tools.ToolSourceID
	URI        string
	OccurredAt time.Time
}
