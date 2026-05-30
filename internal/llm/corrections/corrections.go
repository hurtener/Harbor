// Package corrections is Harbor's provider correction layer (Phase 34
// — RFC §6.5). It sits BETWEEN the runtime and the `llm.LLMClient`
// driver, rewriting `CompleteRequest`s per a per-model
// `CorrectionsProfile` before delegating, and optionally backfilling
// `CompleteResponse.Usage` from the request's byte length.
//
// Compose order is settled by D-041:
//
//	Open() → corrections.Wrap(safety.New(driver))
//
// — the corrections wrapper is the OUTERMOST layer so the Phase 32
// safety pass sees the POST-correction request (the final outgoing
// payload reaching the driver). Materialization, leak-detection, and
// the token-budget guard all run against the corrected payload, so
// any future correction that grows token count is caught by the
// safety pass.
//
// Five quirks (brief 03 §4, master plan Phase 34 detail block):
//
//  1. Message reordering — NIM and some OpenAI-compatible proxies
//     reject mid-thread `system` messages. `OrderingSystemFirstStrict`
//     collapses all system messages to the front.
//  2. Schema sanitization — OpenAI's structured-output mode requires
//     `additionalProperties:false`+`strict:true` at every nested object
//     schema; other providers reject those keys.
//     `SchemaSanitizationMode` flips between adding / stripping them.
//  3. Reasoning-effort routing — thinking-class models (`o1`, `o3`,
//     `deepseek-reasoner`) consume the effort hint via a
//     provider-specific path. `ReasoningRouteThinking` moves the hint
//     from the top-level field into `req.Extra["reasoning_effort"]`.
//  4. Response-format envelope translation —
//     `ResponseFormatJSONOnly` downgrades `FormatJSONSchema` to
//     `FormatJSONObject` for providers that reject `json_schema`;
//     `ResponseFormatAnthropic` packages the schema into Anthropic's
//     tool-schema envelope inside `req.Extra`.
//  5. Usage backfill — some streaming proxies report `0/0` tokens.
//     `UsageBackfillEnabled` substitutes an estimate computed from
//     request input length + response content length when the driver
//     returns an all-zeros `Usage`.
//
// Scope is structured-output and message-shape correctness only —
// NEVER provider-native tool dispatch (RFC §6.4 / brief 07). The
// Phase 32/33 smoke static guard extends to this package; the banned
// provider-native tool-call API symbols MUST NOT appear here. The
// runtime owns tool dispatch entirely.
//
// Concurrent-reuse (D-025): the wrapper is stateless across calls.
// `Wrap` returns a value that holds an inner `LLMClient` and a
// `ConfigSnapshot`; both are read-only after construction. The per-
// call work allocates fresh slices/maps so concurrent callers never
// share mutable state. The package's `corrections_test.go` pins this
// with N=128 invocations under `-race`.
package corrections

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/llm"
)

// init registers `Wrap` as the corrections-wrapper hook in the `llm`
// package. The production binary blank-imports this package via
// `cmd/harbor/main.go`; the registration fires at process boot and
// `llm.Open()` composes `Wrap(safetyClient(driver))` for every
// `ConfigSnapshot` whose `DisableCorrections` is false (the default).
func init() {
	llm.RegisterCorrectionsWrapper(Wrap)
	// Wave 7b audit FAIL #1: applyDefaults consults this resolver so
	// operator-omitted `OutputMode` flows through to the per-known-
	// provider canonical default — closing the dead-code gap the
	// `DefaultOutputModeFor` symbol had before this hook landed.
	llm.RegisterDefaultOutputModeResolver(DefaultOutputModeFor)
}

// Wrap composes the corrections layer on top of `inner`. The returned
// `llm.LLMClient` rewrites requests per the profile in
// `cfg.ModelProfiles[req.Model].Corrections` (zero-valued profile =
// no-op) and optionally backfills `Usage` on the response.
//
// Production callers wire this through `llm.Open()`; tests that need
// to exercise the wrapper in isolation can call `Wrap` directly.
//
// A nil `inner` panics — composition error caught at boot.
func Wrap(inner llm.LLMClient, cfg llm.ConfigSnapshot) llm.LLMClient {
	if inner == nil {
		panic("corrections.Wrap: inner is nil")
	}
	return &client{inner: inner, cfg: cfg}
}

// client is the corrections wrapper. Immutable after construction.
// The `closed` flag is atomic; the rest is read-only.
type client struct {
	inner  llm.LLMClient
	cfg    llm.ConfigSnapshot
	closed atomic.Bool
}

// Compile-time assertion that *client satisfies llm.LLMClient.
var _ llm.LLMClient = (*client)(nil)

// Complete rewrites the request per the model's profile and
// delegates to the inner client. The inner client (typically a
// `safetyClient`) runs the safety pass on the POST-correction
// request, so any leak / token-budget concern catches the final
// outgoing payload.
//
// Honors ctx cancellation by short-circuiting before each side-
// effect-free transformation step.
func (c *client) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if c.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}

	// Default the model from the snapshot when the caller left it empty.
	// The react planner builds `CompleteRequest{Messages: ...}` without
	// pinning Model (the configured `llm.model` is the natural default —
	// see safety.go). The defaulting MUST happen here, BEFORE the profile
	// lookup, because the corrections layer wraps OUTSIDE the safety pass
	// (D-041) — if we deferred to safety's own default, every
	// profile-keyed correction (reasoning effort, schema mode, envelope
	// shaping) would look up `ModelProfiles[""]`, miss, and silently
	// bypass. That is the bug this fix closes: reasoning effort was never
	// applied because `req.Model` was empty at this layer. The defaulted
	// Model flows downstream on the corrected request, so the safety
	// pass's own default becomes a redundant no-op.
	if req.Model == "" {
		req.Model = c.cfg.Model
	}

	profile, hasProfile := c.cfg.ModelProfiles[req.Model]

	// If no profile exists for the model, run the inner client
	// unmodified — the safety pass will reject with
	// ErrUnsupportedModel; we don't pre-empt that error here so the
	// safety pass keeps its single point of enforcement.
	if !hasProfile {
		return c.inner.Complete(ctx, req)
	}

	// Apply the profile's request-level ReasoningEffort default when the
	// caller did not pin one. The react planner (and repair loop) build
	// `CompleteRequest` without setting ReasoningEffort, so the operator's
	// `model_profiles[<model>].reasoning_effort` is the natural default;
	// an explicit per-call value (e.g. a run-time override) always wins.
	// Without this, the profile knob was dead — the driver never received
	// a reasoning param, and reasoning-capable models returned no
	// reasoning (zero `Kind:"reasoning"` chunks, ReasoningTokens=0). The
	// corrections layer is the right home: it is the one pass that already
	// reads the full profile and owns reasoning routing (the
	// ReasoningRouteThinking step below consumes the populated field).
	if req.ReasoningEffort == "" && profile.ReasoningEffort != "" {
		req.ReasoningEffort = profile.ReasoningEffort
	}

	corrected, err := applyRequestCorrections(req, profile.Corrections)
	if err != nil {
		return llm.CompleteResponse{}, fmt.Errorf("corrections: %w", err)
	}

	resp, err := c.inner.Complete(ctx, corrected)
	if err != nil {
		return resp, err
	}

	if profile.Corrections.UsageBackfillEnabled {
		resp = backfillUsage(resp, corrected, profile)
	}
	return resp, nil
}

// Close marks the wrapper closed and tears down the inner. Idempotent
// — second call is a no-op (inner also idempotent by contract).
func (c *client) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.inner.Close(ctx)
}

// applyRequestCorrections runs each pre-call quirk in order and
// returns a corrected `CompleteRequest` value. The original request
// value is never mutated; all transforms produce fresh slices/maps.
//
// Order is deterministic and matches the data-flow direction:
//
//  1. message reordering   — operates on `req.Messages`
//  2. schema sanitization  — operates on `req.ResponseFormat.JSONSchema`
//  3. reasoning routing    — operates on `req.ReasoningEffort` / `Extra`
//  4. response-format shape — operates on `req.ResponseFormat` / `Extra`
//
// Steps 3 and 4 read `req.Extra`; we always allocate a fresh map so
// concurrent callers cannot share map state.
func applyRequestCorrections(req llm.CompleteRequest, cp llm.CorrectionsProfile) (llm.CompleteRequest, error) {
	out := req
	out.Extra = cloneExtra(req.Extra)

	if cp.MessageOrdering != llm.OrderingDefault {
		reordered, err := normalizeMessages(req.Messages, cp.MessageOrdering)
		if err != nil {
			return req, fmt.Errorf("normalize messages: %w", err)
		}
		out.Messages = reordered
	}

	if req.ResponseFormat != nil && cp.SchemaMode != llm.SchemaDefault &&
		req.ResponseFormat.Kind == llm.FormatJSONSchema &&
		len(req.ResponseFormat.JSONSchema) > 0 {
		sanitized, err := sanitizeSchema(req.ResponseFormat.JSONSchema, cp.SchemaMode)
		if err != nil {
			return req, fmt.Errorf("sanitize schema: %w", err)
		}
		// Copy ResponseFormat so the original value remains immutable.
		rf := *req.ResponseFormat
		rf.JSONSchema = sanitized
		out.ResponseFormat = &rf
	}

	if cp.ReasoningEffortRouting == llm.ReasoningRouteThinking && req.ReasoningEffort != "" {
		// Move the hint from the top-level field into Extra. Bifrost
		// passes Extra opaquely; the per-provider converter (or a
		// future Phase 35 hook) reads `reasoning_effort`.
		out.Extra["reasoning_effort"] = string(req.ReasoningEffort)
		out.ReasoningEffort = ""
	}

	if req.ResponseFormat != nil && cp.ResponseFormatShape != llm.ResponseFormatOpenAI {
		// out.ResponseFormat may have been replaced by the sanitizer
		// step; read from out so we operate on the post-sanitization
		// schema.
		rfPtr := out.ResponseFormat
		if rfPtr == nil {
			rfPtr = req.ResponseFormat
		}
		newRF, err := translateResponseFormatShape(*rfPtr, cp.ResponseFormatShape, out.Extra)
		if err != nil {
			return req, fmt.Errorf("translate response format: %w", err)
		}
		out.ResponseFormat = newRF
	}

	return out, nil
}

// cloneExtra returns a fresh copy of m. Returns a non-nil empty map
// when m is nil so the caller can write into it without re-checking.
func cloneExtra(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// translateResponseFormatShape rewrites `rf` per the operator's
// profile. The OpenAI shape is the default (returned unchanged);
// `json_only` downgrades `FormatJSONSchema` to `FormatJSONObject` and
// stashes the schema in `Extra["schema_hint"]`; `anthropic` packages
// the schema into `Extra["anthropic_tool_schema"]` and clears the
// top-level `ResponseFormat` so bifrost's per-provider converter does
// not also emit it.
func translateResponseFormatShape(rf llm.ResponseFormat, shape llm.ResponseFormatProfile, extra map[string]any) (*llm.ResponseFormat, error) {
	switch shape {
	case llm.ResponseFormatOpenAI:
		out := rf
		return &out, nil
	case llm.ResponseFormatJSONOnly:
		if rf.Kind != llm.FormatJSONSchema {
			out := rf
			return &out, nil
		}
		if len(rf.JSONSchema) > 0 {
			// Stash the schema as a prompted-fallback hint.
			var schemaAny any
			if err := json.Unmarshal(rf.JSONSchema, &schemaAny); err == nil {
				extra["schema_hint"] = schemaAny
			} else {
				// On decode failure keep the raw bytes — the consumer
				// can decide whether to display them. Never silently
				// drop.
				extra["schema_hint"] = string(rf.JSONSchema)
			}
		}
		return &llm.ResponseFormat{Kind: llm.FormatJSONObject}, nil
	case llm.ResponseFormatAnthropic:
		if rf.Kind != llm.FormatJSONSchema || len(rf.JSONSchema) == 0 {
			out := rf
			return &out, nil
		}
		var schemaAny any
		if err := json.Unmarshal(rf.JSONSchema, &schemaAny); err != nil {
			return nil, fmt.Errorf("anthropic envelope: decode schema: %w", err)
		}
		extra["anthropic_tool_schema"] = schemaAny
		// Clear the top-level field so bifrost's per-provider
		// translator does not also emit a redundant OpenAI-style
		// envelope.
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown ResponseFormatProfile %q", shape)
	}
}

// backfillUsage substitutes a synthetic `Usage` (and, if a
// `CostOverrides` table is configured, a synthetic `Cost`) when the
// driver returned all-zero usage. Some streaming proxies report
// `0/0` despite returning content; this lets the operator quantify
// activity even when the upstream pricing surface is silent.
//
// The estimator is the byte-length-based one from the safety pass
// (chars/4 + per-message overhead) — kept consistent so operator
// dashboards see the same numbers across the safety net and the
// backfill.
func backfillUsage(resp llm.CompleteResponse, req llm.CompleteRequest, profile llm.ModelProfile) llm.CompleteResponse {
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 || resp.Usage.TotalTokens > 0 {
		return resp
	}
	prompt := estimateRequestTokens(req)
	completion := estimateStringTokens(resp.Content)
	total := prompt + completion
	resp.Usage.PromptTokens = prompt
	resp.Usage.CompletionTokens = completion
	resp.Usage.TotalTokens = total
	if profile.CostOverrides != nil && resp.Cost.TotalCost == 0 {
		const million = 1_000_000.0
		resp.Cost.InputTokensCost = float64(prompt) / million * profile.CostOverrides.InputPer1M
		resp.Cost.OutputTokensCost = float64(completion) / million * profile.CostOverrides.OutputPer1M
		resp.Cost.TotalCost = resp.Cost.InputTokensCost + resp.Cost.OutputTokensCost
		if profile.CostOverrides.Currency != "" {
			resp.Cost.Currency = profile.CostOverrides.Currency
		} else {
			resp.Cost.Currency = "USD"
		}
	}
	return resp
}
