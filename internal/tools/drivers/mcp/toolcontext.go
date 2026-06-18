package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/hurtener/Harbor/internal/tools"
)

// CapturedToolContext is the input a ToolContextCapturer persists when an
// invoked MCP tool declared an interactive app. It is the "Data Delivery"
// half of the MCP Apps lifecycle: the input arguments and the lowered
// result that produced the app, keyed by the stable ToolCallID so a
// Protocol client (the rendered app) can fetch them later. The capture
// rides the SAME ctx the tool call ran under, so the persisted record is
// scoped to the caller's identity triple.
type CapturedToolContext struct {
	// ServerID is the MCP server (source id) the tool belongs to. Paired
	// with ToolCallID it forms the lookup key the host reads back through.
	ServerID tools.ToolSourceID
	// ToolCallID is the stable, collision-free per-invocation id minted by
	// ToolCallID (the content hash). It is the same value carried on the
	// discovery event and projected onto the app reference.
	ToolCallID string
	// Tool is the server-side tool name (display metadata only — the
	// lookup keys on ServerID + ToolCallID, never the tool name).
	Tool string
	// Input is the raw JSON argument object the tool was invoked with.
	Input json.RawMessage
	// Result is the JSON-encoded lowered tool result.
	Result json.RawMessage
	// IsError reports whether the tool returned a server-side error result.
	IsError bool
}

// ToolContextCapturer persists the tool context behind a declared MCP App
// so the host can deliver it to the rendered app. It is an optional seam
// on the MCP Config: when set, the driver calls Capture from the
// tool-invocation path whenever a result declares a `ui://` app. The
// implementation lives in the runtime (over the StateStore + ArtifactStore
// — heavy-aware), wired at boot; the driver holds only this narrow
// interface so it never imports the runtime concretes.
//
// Capture is best-effort relative to the tool call: a Capture error is
// surfaced to the caller (the invocation path logs it loudly and never
// silently swallows it, CLAUDE.md §13), but it does not fail the tool call
// itself — the planner still receives the tool result.
type ToolContextCapturer interface {
	Capture(ctx context.Context, in CapturedToolContext) error
}

// ToolCallID mints the stable, collision-free identifier for one tool
// invocation that declared an app. It is a deterministic content hash of
// the run / server / tool / args — NOT a counter or any mutable Provider
// field (the Provider is a compiled artifact, immutable after
// construction; per-call identity rides ctx, CLAUDE.md §5). Two
// invocations with distinct (run, server, tool, args) tuples never
// collide; the same tuple within a run is idempotent by construction (it
// re-derives the same id and overwrites the same captured slot).
func ToolCallID(runID, serverID, tool string, args json.RawMessage) string {
	h := sha256.New()
	// Newline-separate each field so distinct field boundaries cannot alias
	// (e.g. {"ab","c"} vs {"a","bc"}). Args, which may itself contain
	// newlines, is written last (unseparated) so no trailing field can be
	// confused with a separator.
	for _, part := range []string{runID, serverID, tool} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{'\n'})
	}
	_, _ = h.Write(args)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}
