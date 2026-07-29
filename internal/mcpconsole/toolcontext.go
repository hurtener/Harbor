package mcpconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/state"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// ToolContextStore is the runtime-side store behind the MCP Apps "Data
// Delivery" lifecycle: the WRITE half (Capture) persists the input +
// lowered result behind a declared `ui://` app at the tool-invocation
// site, and the READ half (Load) projects it back for
// `mcp.apps.tool_context`. It rides the EXISTING StateStore — all three
// drivers (in-mem / SQLite / Postgres) and identity isolation come free —
// keyed by the caller's identity triple + a per-call Kind; the heavy
// content (a large result) offloads to the ArtifactStore by reference
// through the shared loud-bypass path, so a bulky context never bloats a
// StateRecord and never reaches the LLM.
//
// The record is scoped to the (tenant, user, session) triple with an EMPTY
// RunID: the captured context is session-scoped (a rendered app reads it
// within the session that produced it), so the read — which knows the
// triple but not necessarily the producing run — resolves it. The
// per-invocation tool-call id disambiguates calls within the session via
// the Kind. A cross-identity Load returns not-found by construction
// (StateStore.Load filters by the triple).
//
// Concurrent reuse: ToolContextStore is an immutable adapter — the wrapped
// StateStore / ArtifactStore / Bus are concurrent-safe compiled artifacts
// and the adapter adds no mutable state. Per-call identity rides ctx
// (CLAUDE.md §5).
type ToolContextStore struct {
	state     state.StateStore
	store     artifacts.ArtifactStore
	bus       events.EventBus
	threshold int
	clock     func() time.Time
}

// ToolContextDeps bundles the collaborators the ToolContextStore rides on.
// State, Store, and Bus are mandatory — a nil fails closed at
// construction. Threshold ≤ 0 falls back to the Console inline-payload
// bound; Clock defaults to time.Now.
type ToolContextDeps struct {
	// State is the StateStore the captured context persists through.
	// Mandatory.
	State state.StateStore
	// Store is the ArtifactStore a heavy (input or result) payload offloads
	// to by reference. Mandatory.
	Store artifacts.ArtifactStore
	// Bus carries the `mcp.resource_offloaded` loud bypass event on a heavy
	// offload. Mandatory.
	Bus events.EventBus
	// Threshold is the inline-payload bound in bytes. ≤ 0 falls back to
	// the Console inline-payload bound. The wiring passes that pinned
	// bound rather than the operator's LLM-context heavy-output
	// threshold: a captured tool context is read back by the Console,
	// never replayed into a prompt.
	Threshold int
	// Clock returns the current wall-clock time. Optional — defaults to
	// time.Now.
	Clock func() time.Time
}

// ErrToolContextMisconfigured — NewToolContextStore was called with a
// missing mandatory dependency. Fails closed.
var ErrToolContextMisconfigured = errors.New("mcpconsole: ToolContextStore missing a mandatory dependency")

// compile-time assertion: ToolContextStore satisfies the driver capturer
// seam.
var _ mcp.ToolContextCapturer = (*ToolContextStore)(nil)

// NewToolContextStore builds the MCP Apps tool-context store. State, Store,
// and Bus are mandatory; a nil fails loud.
func NewToolContextStore(deps ToolContextDeps) (*ToolContextStore, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("%w: State is nil", ErrToolContextMisconfigured)
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: Store is nil", ErrToolContextMisconfigured)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Bus is nil", ErrToolContextMisconfigured)
	}
	threshold := deps.Threshold
	if threshold <= 0 {
		threshold = config.DefaultConsoleInlinePayloadBytes
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &ToolContextStore{
		state:     deps.State,
		store:     deps.Store,
		bus:       deps.Bus,
		threshold: threshold,
		clock:     clock,
	}, nil
}

// toolContextKindPrefix is the StateStore Kind namespace for captured MCP
// App tool contexts. The full Kind is
// "mcp.apps.tool_context/<serverID>/<toolCallID>".
const toolContextKindPrefix = "mcp.apps.tool_context"

// toolContextKind builds the per-call StateStore Kind.
func toolContextKind(serverID, toolCallID string) string {
	return fmt.Sprintf("%s/%s/%s", toolContextKindPrefix, serverID, toolCallID)
}

// toolContextEnvelope is the JSON payload persisted in a StateRecord. Each
// of input / result is inline-or-by-reference: a heavy payload offloads to
// the ArtifactStore and the envelope carries only the small reference, so
// the StateRecord itself stays small regardless of payload size.
type toolContextEnvelope struct {
	Tool         string                           `json:"tool"`
	IsError      bool                             `json:"is_error"`
	InlineInput  json.RawMessage                  `json:"inline_input,omitempty"`
	InputRef     *protocol.MCPResourceArtifactRow `json:"input_ref,omitempty"`
	InlineResult json.RawMessage                  `json:"inline_result,omitempty"`
	ResultRef    *protocol.MCPResourceArtifactRow `json:"result_ref,omitempty"`
}

// Capture implements mcp.ToolContextCapturer. It persists the tool context
// (input + lowered result) keyed by the ctx identity triple + the per-call
// Kind. Each payload below the inline-payload bound rides inline in the
// envelope; at or above it, the payload offloads to the ArtifactStore by
// reference (the shared loud-bypass path) and the envelope carries the
// reference. A missing identity fails closed (CLAUDE.md §6).
func (s *ToolContextStore) Capture(ctx context.Context, in mcp.CapturedToolContext) error {
	id, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("mcpconsole: tool-context capture: %w", mcp.ErrIdentityMissing)
	}
	env := toolContextEnvelope{Tool: in.Tool, IsError: in.IsError}

	inlineInput, inputRef, err := s.shape(ctx, in.Input, in.Tool, in.ToolCallID, "input")
	if err != nil {
		return err
	}
	env.InlineInput, env.InputRef = inlineInput, inputRef

	inlineResult, resultRef, err := s.shape(ctx, in.Result, in.Tool, in.ToolCallID, "result")
	if err != nil {
		return err
	}
	env.InlineResult, env.ResultRef = inlineResult, resultRef

	bytes, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("mcpconsole: encode tool context: %w", err)
	}
	rec := state.StateRecord{
		ID: state.NewEventID(),
		// Session-scoped: empty RunID so the read (which need not know the
		// producing run) resolves the record by the triple + Kind.
		Identity:  identity.Quadruple{Identity: id},
		Kind:      toolContextKind(string(in.ServerID), in.ToolCallID),
		Bytes:     bytes,
		UpdatedAt: s.clock(),
	}
	if saveErr := s.state.Save(ctx, rec); saveErr != nil {
		return fmt.Errorf("mcpconsole: persist tool context: %w", saveErr)
	}
	return nil
}

// shape decides the inline-or-by-reference form of one payload half. An
// empty payload normalises to inline `null` (a valid, tiny JSON value).
func (s *ToolContextStore) shape(ctx context.Context, payload json.RawMessage, tool, toolCallID, half string) (json.RawMessage, *protocol.MCPResourceArtifactRow, error) {
	if len(payload) == 0 {
		return json.RawMessage("null"), nil, nil
	}
	if len(payload) < s.threshold {
		return payload, nil, nil
	}
	filename := fmt.Sprintf("tool-context-%s-%s.json", half, toolCallID)
	source := fmt.Sprintf("tool-context-%s:%s", half, tool)
	row, err := offloadHeavy(ctx, s.store, s.bus, s.clock, payload, "application/json", source, filename)
	if err != nil {
		return nil, nil, err
	}
	return nil, row, nil
}

// Load resolves a captured tool context for `mcp.apps.tool_context`,
// scoped to the ctx identity triple. An unknown or cross-identity
// (serverID, toolCallID) returns a wrapped not-found error whose marker
// the Protocol surface maps to CodeNotFound — existence is never revealed
// across identities. A missing identity fails closed.
func (s *ToolContextStore) Load(ctx context.Context, serverID, toolCallID string) (protocol.AppToolContextRow, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return protocol.AppToolContextRow{}, fmt.Errorf("mcpconsole: tool-context load: %w", mcp.ErrIdentityMissing)
	}
	q := identity.Quadruple{Identity: id}
	rec, err := s.state.Load(ctx, q, toolContextKind(serverID, toolCallID))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			// Wrap the Protocol's not-found sentinel so the wire edge classifies
			// on THIS accessor's verdict rather than on the rendered text (see
			// mcpconsole.markNotFound for why text classification was unsafe).
			return protocol.AppToolContextRow{}, fmt.Errorf("%w: mcpconsole: tool context not found (server %q, call %q): %w",
				protocol.ErrAccessorNotFound, serverID, toolCallID, err)
		}
		return protocol.AppToolContextRow{}, fmt.Errorf("mcpconsole: load tool context: %w", err)
	}
	var env toolContextEnvelope
	if uErr := json.Unmarshal(rec.Bytes, &env); uErr != nil {
		return protocol.AppToolContextRow{}, fmt.Errorf("mcpconsole: decode tool context: %w", uErr)
	}
	return protocol.AppToolContextRow{
		Tool:    env.Tool,
		IsError: env.IsError,
		Input:   payloadRow(env.InlineInput, env.InputRef),
		Result:  payloadRow(env.InlineResult, env.ResultRef),
	}, nil
}

// payloadRow projects one envelope half onto the runtime row: a reference
// when offloaded, inline JSON otherwise.
func payloadRow(inline json.RawMessage, ref *protocol.MCPResourceArtifactRow) protocol.AppToolContextPayloadRow {
	if ref != nil {
		return protocol.AppToolContextPayloadRow{Artifact: ref}
	}
	return protocol.AppToolContextPayloadRow{Inline: inline}
}
