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
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
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

func init() {
	events.RegisterEventType(EventTypeMCPResourceUpdated)
	events.RegisterEventType(EventTypeMCPResourceOffloaded)
	events.RegisterEventType(EventTypeMCPAppAvailable)
}

// AppAvailablePayload is the typed payload for EventTypeMCPAppAvailable.
// SafePayload: no caller-controlled MCP content survives on the payload —
// only the server source id, the `ui://` resource URI, the per-result
// display-mode hint, the default-deny raw-HTML trust posture, and the
// actor identity quadruple (its RunID correlates the discovery to the
// turn that produced it).
type AppAvailablePayload struct {
	events.SafeSealed
	// Identity scopes the discovery to the (tenant, user, session) triple
	// the tool ran under; its RunID is the turn-correlation key.
	Identity identity.Quadruple
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
