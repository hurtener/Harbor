// Input-artifact disposition observability (Phase 84b — D-189).
//
// The disposition resolver and the materializer are pure — they
// RETURN the winning precedence layer and any degradation fact as
// typed values; the caller logs and emits. This file is the canonical
// event vocabulary the shared run-loop helper
// [ResolveInputArtifacts] emits on behalf of every caller (cmd /
// devstack / a headless consumer that supplies an emit closure), so
// operators can see which disposition fired, why (which layer won),
// and that the runtime never silently overrode them.
package runctx

import (
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// EventTypeInputDispositionResolved is published once per input
// artifact when the run loop resolves the attachment's disposition
// (Phase 84b — D-189). The payload carries the resolved disposition,
// the precedence layer that won (caller_hint / agent_policy /
// runtime_default), and — when the resolved disposition could not be
// honoured — the typed degradation fact (e.g. an unknown
// `tool:<name>`, or `inline` for a non-image MIME). A resolved
// `provider_native` is honoured as-is; the provider upload itself is
// observable via the `llm.provider_file.uploaded` event.
const EventTypeInputDispositionResolved events.EventType = "task.input_disposition.resolved"

func init() {
	events.RegisterEventType(EventTypeInputDispositionResolved)
}

// InputDispositionResolvedPayload is the
// `task.input_disposition.resolved` event payload. SafePayload — the
// fields are artifact ref / MIME / disposition / layer metadata plus
// the identity quadruple; no operator content and no tool arguments,
// so the audit redactor bypasses it and subscribers keep typed access
// (the `llm.image.materialized` precedent).
type InputDispositionResolvedPayload struct {
	events.SafeSealed `json:"-"`

	// Identity is the run's identity quadruple.
	Identity identity.Quadruple `json:"identity"`
	// TaskID is the task whose input artifact was resolved.
	TaskID string `json:"task_id"`
	// ArtifactID is the input artifact's content-addressed id.
	ArtifactID string `json:"artifact_id"`
	// MIME is the artifact's IANA media type.
	MIME string `json:"mime"`
	// Disposition is the EFFECTIVE disposition the materializer will
	// honour (`ref` / `inline` / `provider_native` / `tool:<name>`).
	Disposition string `json:"disposition"`
	// Layer is the precedence layer that won the resolution
	// (`caller_hint` / `agent_policy` / `runtime_default`).
	Layer string `json:"layer"`
	// Degraded is true when the resolved disposition could not be
	// honoured and fell back to `ref`.
	Degraded bool `json:"degraded"`
	// DegradedFrom is the disposition that was requested when
	// Degraded is true ("" otherwise).
	DegradedFrom string `json:"degraded_from,omitempty"`
	// DegradationReason is the typed cause when Degraded is true
	// (`unknown_tool` / `inline_unsupported_mime` /
	// `invalid_disposition`).
	DegradationReason string `json:"degradation_reason,omitempty"`
	// Tool is the unknown tool name when DegradationReason is
	// `unknown_tool`.
	Tool string `json:"tool,omitempty"`
	// OccurredAt is the resolution wall-clock instant.
	OccurredAt time.Time `json:"occurred_at"`
}
