package output

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// downgradeClient implements the Phase 35 wrapper. Immutable after
// construction; the closed flag is atomic; the wrapped inner client
// owns the per-attempt heavy lifting.
type downgradeClient struct {
	deps   llm.Deps
	inner  llm.LLMClient
	cfg    llm.ConfigSnapshot
	closed atomic.Bool
}

var _ llm.LLMClient = (*downgradeClient)(nil)

func newDowngradeClient(inner llm.LLMClient, cfg llm.ConfigSnapshot, deps llm.Deps) *downgradeClient {
	return &downgradeClient{inner: inner, cfg: cfg, deps: deps}
}

// maxDowngradeAttempts caps the chain (initial + 2 downgrades = 3).
const maxDowngradeAttempts = 3

// Complete runs the downgrade chain. The first attempt uses the
// caller's profile-selected (or operator-overridden) `OutputMode`;
// each schema-class failure walks the chain one step.
//
// Honors ctx cancellation between attempts.
func (d *downgradeClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}

	// Resolve the starting mode from the profile. If no profile or
	// the mode is unset, the wrapper is a pure pass-through — the
	// inner safety pass already rejects unknown models, so we don't
	// duplicate that check here.
	profile, hasProfile := d.cfg.ModelProfiles[req.Model]
	if !hasProfile || profile.OutputMode == llm.OutputModeUnset {
		return d.inner.Complete(ctx, req)
	}

	// If the caller did not request structured output, downgrade is
	// inapplicable. Pass through.
	if req.ResponseFormat == nil || req.ResponseFormat.Kind == llm.FormatText {
		return d.inner.Complete(ctx, req)
	}

	id := identityFromCtx(ctx)
	mode := profile.OutputMode

	attempts := make([]attemptRecord, 0, maxDowngradeAttempts)
	var lastErr error

	for range maxDowngradeAttempts {
		if err := ctx.Err(); err != nil {
			return llm.CompleteResponse{}, err
		}

		shaped, err := shapeRequestForMode(req, mode)
		if err != nil {
			return llm.CompleteResponse{}, fmt.Errorf("output: shape request for mode %q: %w", mode, err)
		}

		resp, err := d.inner.Complete(ctx, shaped)
		if err == nil {
			return resp, nil
		}

		attempts = append(attempts, attemptRecord{Mode: mode, Err: err})
		lastErr = err

		// Only schema-class failures trigger a downgrade. Any other
		// error (transient / auth / 5xx) terminates the chain
		// immediately.
		if !llm.IsInvalidJSONSchemaError(err) {
			return llm.CompleteResponse{}, err
		}

		nextMode, hasNext := nextDowngrade(mode)
		if !hasNext {
			break
		}
		emitModeDowngraded(ctx, d.deps.Bus, id, req.Model, mode, nextMode, err)
		mode = nextMode
	}

	// Chain exhausted. Surface the wrapped chain.
	return llm.CompleteResponse{}, fmt.Errorf("%w: %s", llm.ErrDowngradeExhausted, summariseAttempts(attempts, lastErr))
}

// Close marks the wrapper closed and tears down the inner. Idempotent.
func (d *downgradeClient) Close(ctx context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	return d.inner.Close(ctx)
}

// attemptRecord captures one step in the downgrade chain for chain
// summaries on exhaustion. Bounded — only used for the final error.
type attemptRecord struct {
	Err  error
	Mode llm.OutputMode
}

// nextDowngrade returns the next `OutputMode` in the chain. The chain
// is:
//
//	Native   → Prompted
//	Tools    → Prompted
//	Prompted → "text" (sentinel; no next)
//
// Returns ok=false when no further downgrade is possible.
func nextDowngrade(cur llm.OutputMode) (llm.OutputMode, bool) {
	switch cur {
	case llm.OutputModeNative, llm.OutputModeTools:
		return llm.OutputModePrompted, true
	case llm.OutputModePrompted:
		// Already at the prompted floor — the next step is text,
		// represented by clearing ResponseFormat. We surface this via
		// the empty-output-mode return value AND the shaped request
		// drops `ResponseFormat`.
		return llm.OutputMode("text"), true
	case llm.OutputMode("text"):
		return "", false
	default:
		return "", false
	}
}

// shapeRequestForMode rewrites `req` so its `ResponseFormat` matches
// the wrapper's current mode. Operates on a copied value; the original
// request is never mutated.
func shapeRequestForMode(req llm.CompleteRequest, mode llm.OutputMode) (llm.CompleteRequest, error) {
	out := req

	switch mode {
	case llm.OutputModeNative:
		// Pass-through. Validate that the caller actually asked for a
		// schema-shape; if they asked for `json_object` already we
		// don't promote to schema (caller knows their schema).
		return out, nil

	case llm.OutputModeTools:
		// Encode the schema (if any) into the Harbor-side prompted
		// envelope. The envelope is a JSON object asking the model to
		// produce `{"name":"respond_with","arguments":{...}}`.
		// `ResponseFormat` is coerced to FormatJSONObject so the
		// provider returns a JSON object; the runtime parses the
		// nested arguments locally.
		out.ResponseFormat = &llm.ResponseFormat{Kind: llm.FormatJSONObject}
		if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
			out.Messages = appendSystemMessage(req.Messages,
				renderToolsEnvelopeInstruction(req.ResponseFormat.JSONSchema))
		}
		return out, nil

	case llm.OutputModePrompted:
		// Coerce to JSONObject + inline the schema as system text.
		out.ResponseFormat = &llm.ResponseFormat{Kind: llm.FormatJSONObject}
		if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
			out.Messages = appendSystemMessage(req.Messages,
				renderPromptedInstruction(req.ResponseFormat.JSONSchema))
		}
		return out, nil

	case llm.OutputMode("text"):
		// Clear ResponseFormat so the provider returns plain text.
		out.ResponseFormat = nil
		// Still surface the schema as guidance, but don't enforce.
		if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
			out.Messages = appendSystemMessage(req.Messages,
				renderTextFallbackInstruction(req.ResponseFormat.JSONSchema))
		}
		return out, nil

	default:
		return req, fmt.Errorf("unknown OutputMode %q", mode)
	}
}

// appendSystemMessage returns a fresh slice with a new system message
// appended at the front. Mutates neither the input slice nor any
// shared backing array.
func appendSystemMessage(msgs []llm.ChatMessage, text string) []llm.ChatMessage {
	clone := make([]llm.ChatMessage, 0, len(msgs)+1)
	t := text
	clone = append(clone, llm.ChatMessage{
		Role:    llm.RoleSystem,
		Content: llm.Content{Text: &t},
	})
	clone = append(clone, msgs...)
	return clone
}

// renderToolsEnvelopeInstruction builds the system-prompt instruction
// for `OutputModeTools`. The model is asked to emit a single JSON
// object describing a "respond_with" tool call whose arguments match
// the supplied schema.
func renderToolsEnvelopeInstruction(schema []byte) string {
	var schemaStr string
	if v, err := compactJSON(schema); err == nil {
		schemaStr = v
	} else {
		schemaStr = string(schema)
	}
	return "Respond ONLY with a single JSON object of shape " +
		`{"name":"respond_with","arguments":<args>}` +
		" where `arguments` matches this JSON Schema: " + schemaStr +
		". Do not emit any text outside this JSON object."
}

// renderPromptedInstruction builds the system-prompt instruction for
// `OutputModePrompted`. The model is asked to emit a JSON object
// matching the supplied schema; the response is parsed as
// FormatJSONObject.
func renderPromptedInstruction(schema []byte) string {
	var schemaStr string
	if v, err := compactJSON(schema); err == nil {
		schemaStr = v
	} else {
		schemaStr = string(schema)
	}
	return "Respond with a JSON object matching this JSON Schema: " + schemaStr +
		". Do not emit any text outside the JSON object."
}

// renderTextFallbackInstruction surfaces the schema as guidance for
// the text fallback. We don't enforce ResponseFormat here — the chain
// is at its terminal step and the next attempt would return
// ErrDowngradeExhausted on schema failure.
func renderTextFallbackInstruction(schema []byte) string {
	var schemaStr string
	if v, err := compactJSON(schema); err == nil {
		schemaStr = v
	} else {
		schemaStr = string(schema)
	}
	return "Where useful, structure your response as JSON matching this schema: " + schemaStr +
		". Plain text is acceptable when JSON is not natural."
}

// compactJSON re-encodes the raw schema bytes through encoding/json
// to strip whitespace. Best-effort; returns the original string on
// error.
func compactJSON(b []byte) (string, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// summariseAttempts builds the chain history string surfaced in the
// `ErrDowngradeExhausted` wrap. Bounded — each attempt contributes
// `mode=<m> err=<truncated>`.
func summariseAttempts(attempts []attemptRecord, last error) string {
	if len(attempts) == 0 && last != nil {
		return last.Error()
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, fmt.Sprintf("mode=%s err=%s", a.Mode, truncate(a.Err.Error(), 160)))
	}
	return strings.Join(parts, " | ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// identityFromCtx mirrors the safety pass's `identityQuad`. We don't
// export the helper from the `llm` package; the duplication is
// tiny and keeps the output package self-contained.
func identityFromCtx(ctx context.Context) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	id, _ := identity.From(ctx)
	return identity.Quadruple{Identity: id}
}

// emitModeDowngraded publishes the `llm.mode_downgraded` event.
// Best-effort; never block on the bus.
func emitModeDowngraded(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, from, to llm.OutputMode, cause error) {
	if bus == nil {
		return
	}
	fromKind, toKind := outputModeToResponseFormatKind(from), outputModeToResponseFormatKind(to)
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort event emit; publish failure must not fail downgrade
		Type:       llm.EventTypeModeDowngraded,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: llm.ModeDowngradedPayload{
			Identity:   id,
			Model:      model,
			FromMode:   from,
			ToMode:     to,
			From:       fromKind,
			To:         toKind,
			Reason:     truncate(cause.Error(), 256),
			OccurredAt: time.Now(),
		},
	})
}

// outputModeToResponseFormatKind maps Harbor's `OutputMode` enum to
// the resolved wire-level `ResponseFormatKind` for the event payload's
// backward-compat fields.
func outputModeToResponseFormatKind(m llm.OutputMode) llm.ResponseFormatKind {
	switch m {
	case llm.OutputModeNative:
		return llm.FormatJSONSchema
	case llm.OutputModeTools, llm.OutputModePrompted:
		return llm.FormatJSONObject
	case llm.OutputMode("text"):
		return llm.FormatText
	default:
		return ""
	}
}
