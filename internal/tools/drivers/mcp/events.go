// Package mcp is Harbor's Model Context Protocol (MCP) southbound
// driver. It implements `tools.ToolProvider` against a remote MCP
// server, exposing the server's tools / resources / prompts as
// Harbor `Tool` entries (RFC §6.4). Three wire transports are
// supported (stdio, SSE, streamable-HTTP) with auto-detect.
//
// Concurrent reuse (D-025): a constructed *Provider is safe to share
// across N concurrent goroutines after Connect returns. All per-call
// state lives on the goroutine stack + the request `ctx`; descriptor
// fields are immutable after Discover.
//
// Identity (RFC §4): the (tenant, user, session) triple is forwarded
// to the remote MCP server in the request's `_meta` map so trust
// signals flow across the seam.
//
// Reliability shell (D-024): every Invoke runs inside
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
// for a URI the driver previously subscribed to (Phase 28). The
// payload is SafePayload by construction — the URI is operator-
// trust-equivalent (it originates from an operator-configured MCP
// server) and the source ID is operator-supplied.
const EventTypeMCPResourceUpdated events.EventType = "mcp.resource_updated"

func init() {
	events.RegisterEventType(EventTypeMCPResourceUpdated)
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
	OccurredAt time.Time
	Identity   identity.Quadruple
	Source     tools.ToolSourceID
	URI        string
}
