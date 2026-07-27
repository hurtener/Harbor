package llm

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// safetyClient wraps a `Driver` and enforces the runtime-wide
// invariants every `Complete` MUST respect (AGENTS.md
// §6 rule 9):
//
//  1. Identity is mandatory — missing → ErrIdentityMissing.
//  2. Auto-materialize oversize DataURL content to ArtifactRef.
//
// Emits `llm.image.materialized`.
//  3. Assert no raw heavy content survived (AGENTS.md §13).
//     Emits `llm.context_leak` on failure.
//  4. Estimate total tokens against ModelProfile.ContextWindowTokens
//     and fail with ErrContextWindowExceeded when within the
//     ContextWindowReserve margin. Emits
//     `llm.context_window_exceeded` on failure.
//
// The wrapper is mandatory by construction — `registry.Open` returns
// a `*safetyClient`, not a raw `Driver`. Drivers that need to test
// against a bare `Driver` can construct one in their own package
// tests, but production calls always route through here.
//
// Concurrent-reuse contract: the wrapper is stateless across
// calls. The `closed` flag is `atomic.Bool` for the idempotent Close
// path; `cfg` is read-only after construction.
type safetyClient struct {
	driver Driver
	cfg    ConfigSnapshot
	deps   Deps
	closed atomic.Bool
}

// Compile-time assertion that *safetyClient satisfies LLMClient.
var _ LLMClient = (*safetyClient)(nil)

func newSafetyClient(d Driver, cfg ConfigSnapshot, deps Deps) *safetyClient {
	return &safetyClient{driver: d, cfg: cfg, deps: deps}
}

// Complete runs the safety pass + the driver. The safety pass is
// non-bypassable through this code path.
func (c *safetyClient) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	if c.closed.Load() {
		return CompleteResponse{}, ErrClientClosed
	}
	if !HasIdentity(ctx) {
		return CompleteResponse{}, ErrIdentityMissing
	}
	id := identityQuad(ctx)

	// Fill in the agent-configured default Model when the caller did
	// not pin one. The react planner builds
	// `CompleteRequest{Messages: ...}` without setting Model — the
	// configured `llm.model` is the natural default. A caller that
	// explicitly pins Model (multi-model agents, posture sub-clients)
	// keeps their pin. Surfaced by the v1.1 operator-validation work:
	// before this default, every real-bifrost run failed at step 0
	// with `CompleteRequest.Model is empty` because the mock LLM
	// driver used in integration tests does not enforce Model and the
	// gap never surfaced under real LLM workloads.
	if req.Model == "" {
		req.Model = c.cfg.Model
	}

	// Step 0: structural validation. Cheap; surface obviously-broken
	// requests before doing real work.
	if err := validateRequest(req); err != nil {
		return CompleteResponse{}, err
	}

	// Profile lookup. Required for the token-budget guard; missing
	// is a config error the operator should fix.
	profile, ok := c.cfg.ModelProfiles[req.Model]
	if !ok {
		return CompleteResponse{}, fmt.Errorf("%w: model=%q (configure ModelProfiles[%q] in harbor.yaml)",
			ErrUnsupportedModel, req.Model, req.Model)
	}

	// Step 1: auto-materialize. Rewrites oversize DataURLs in-place
	// on a copied request value.
	materialized, err := materializeRequest(ctx, req, c.deps.Artifacts, c.deps.Bus, c.cfg.HeavyOutputThreshold, id)
	if err != nil {
		return CompleteResponse{}, err
	}

	// Step 2: leak detection. Walks the materialized request and
	// asserts no raw heavy content survived.
	if site, sz, ok := findContextLeak(materialized, c.cfg.HeavyOutputThreshold); ok {
		emitContextLeak(ctx, c.deps.Bus, id, req.Model, site, sz, c.cfg.HeavyOutputThreshold)
		return CompleteResponse{}, fmt.Errorf("%w: site=%s size=%d threshold=%d", ErrContextLeak, site, sz, c.cfg.HeavyOutputThreshold)
	}

	// Step 3: token-budget guard.
	estimated := estimateTokens(materialized, profile)
	windowCap := profile.ContextWindowTokens
	// Reserve margin: fail when estimated >= windowCap * (1 - reserve).
	// Equivalently: fail when (windowCap - estimated) < windowCap * reserve.
	effectiveCap := int(float64(windowCap) * (1.0 - c.cfg.ContextWindowReserve))
	if estimated >= effectiveCap {
		emitContextWindowExceeded(ctx, c.deps.Bus, id, req.Model, estimated, windowCap, c.cfg.ContextWindowReserve)
		return CompleteResponse{}, fmt.Errorf("%w: estimated=%d cap=%d reserve=%g (effective_cap=%d)",
			ErrContextWindowExceeded, estimated, windowCap, c.cfg.ContextWindowReserve, effectiveCap)
	}

	// Honour ctx cancellation between steps.
	if err := ctx.Err(); err != nil {
		return CompleteResponse{}, err
	}

	// Drive the underlying driver. Per-call timeout: if the caller's
	// ctx has no deadline, layer one in defensively. Prefer the
	// operator-configured `llm.timeout` (`c.cfg.Timeout`) when > 0;
	// fall back to `defaultRequestTimeout` only when the operator
	// left it zero. (item 5 — the operator-facing yaml
	// field was wired through `ConfigSnapshot.Timeout` but the
	// safety wrapper ignored it and always used the 5-minute floor,
	// so any operator who tightened `llm.timeout` saw no effect on
	// the safety-net's defensive deadline.)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := c.cfg.Timeout
		if timeout <= 0 {
			timeout = defaultRequestTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, err := c.driver.Complete(ctx, materialized)
	if err != nil {
		// Pass the driver's response through UNCHANGED alongside the
		// error — an errored attempt still carries a provider-reported
		// `Cost` the outer retry/downgrade attempt-cost tap folds into
		// governance accounting. Zeroing it here would silently drop
		// intermediate-attempt spend. No cost observability emit on the
		// error path (the emit rides successful driver-level completions,
		// matching the old bifrost cadence).
		return resp, err
	}

	// Driver-neutral cost/usage/model observability emit. The safety
	// wrapper is the innermost mandatory band `Open` composes around
	// EVERY driver, so `llm.cost.recorded` fires for every provider —
	// bifrost, the dev-posture mock, any future driver — with no
	// per-driver emit to maintain. One event per driver-level
	// completion: because the retry-with-feedback wrapper composes
	// OUTSIDE this mandatory inner band, a retried call routes through
	// here once per attempt, so the emit preserves the per-attempt
	// cadence the
	// per-call attempt-cost governance tap already assumes. The emit is
	// observability-only — cost accounting stays in-band synchronous in
	// the governance PostCall.
	emitCostRecorded(ctx, c.deps.Bus, id, req.Model, resp.Cost, resp.Usage, profile.ContextWindowTokens)
	return resp, nil
}

// Close marks the client closed and tears down the driver.
// Idempotent — second call is a no-op (driver also idempotent by
// contract).
func (c *safetyClient) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.driver.Close(ctx)
}

// validateRequest checks structural invariants the safety pass relies
// on before doing real work. Returns ErrInvalidContent on malformed
// Content; ErrInvalidConfig for empty Model; ErrOrphanToolCall when
// an assistant `tool_calls` block is not followed by matching
// `RoleTool` messages.
//
// An assistant message with `len(ToolCalls) > 0` is permitted to
// have both `Content.Text` and `Content.Parts` nil — OpenAI's wire
// spec requires `content: null` when `tool_calls` is present, and
// `content: ""` is a 400. The translator emits `content: null` from
// the zero-value Content shape.
func validateRequest(req CompleteRequest) error {
	if req.Model == "" {
		return fmt.Errorf("%w: CompleteRequest.Model is empty", ErrInvalidConfig)
	}
	for mi, m := range req.Messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 &&
			m.Content.Text == nil && m.Content.Parts == nil {
			continue
		}
		if err := validateContent(m.Content); err != nil {
			return fmt.Errorf("messages[%d]: %w", mi, err)
		}
	}
	if err := validateToolCallPairing(req.Messages); err != nil {
		return err
	}
	return nil
}

// validateToolCallPairing enforces the OpenAI-spec wire contract:
// every assistant message that carries `ToolCalls` MUST be followed
// by `RoleTool` messages whose `ToolCallID` matches each
// `ToolCalls[i].ID` (consumed in order, no reuse). A RoleTool
// message not preceded by a matching assistant tool_call, or whose
// `ToolCallID` matches no pending id, is also a violation.
// ToolCallID-less RoleTool messages and assistant messages without
// ToolCalls are inert for this check.
//
// The safety pass is the defense-in-depth gate so any producer that
// violates the pairing contract is rejected loudly here rather than
// silently malforming the upstream request.
func validateToolCallPairing(messages []ChatMessage) error {
	var pendingIDs []string
	pendingAsstIdx := -1
	flush := func() error {
		if len(pendingIDs) == 0 {
			return nil
		}
		return fmt.Errorf("%w: messages[%d] assistant ToolCalls %v had no matching RoleTool messages following",
			ErrOrphanToolCall, pendingAsstIdx, pendingIDs)
	}
	for mi, m := range messages {
		switch m.Role {
		case RoleAssistant:
			if err := flush(); err != nil {
				return err
			}
			if len(m.ToolCalls) > 0 {
				pendingIDs = pendingIDs[:0]
				for _, tc := range m.ToolCalls {
					pendingIDs = append(pendingIDs, tc.ID)
				}
				pendingAsstIdx = mi
			}
		case RoleTool:
			if m.ToolCallID == nil || *m.ToolCallID == "" {
				return fmt.Errorf("%w: messages[%d] RoleTool message is missing ToolCallID",
					ErrOrphanToolCall, mi)
			}
			tid := *m.ToolCallID
			matched := -1
			for i, pid := range pendingIDs {
				if pid == tid {
					matched = i
					break
				}
			}
			if matched < 0 {
				return fmt.Errorf("%w: messages[%d] RoleTool ToolCallID=%q does not match any pending assistant tool_calls (pending=%v)",
					ErrOrphanToolCall, mi, tid, pendingIDs)
			}
			pendingIDs = append(pendingIDs[:matched], pendingIDs[matched+1:]...)
		default:
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// validateContent enforces the Content sum-type invariant: exactly
// one of Text or Parts is set, and every ContentPart's discriminator
// matches its payload.
func validateContent(c Content) error {
	switch {
	case c.Text != nil && c.Parts != nil:
		return fmt.Errorf("%w: both Text and Parts set", ErrInvalidContent)
	case c.Text == nil && c.Parts == nil:
		return fmt.Errorf("%w: neither Text nor Parts set", ErrInvalidContent)
	}
	for pi, p := range c.Parts {
		switch p.Type {
		case PartText:
			// Text is a string field; empty is allowed (some
			// providers legitimately send empty user turns).
		case PartImage:
			if p.Image == nil {
				return fmt.Errorf("%w: Parts[%d].Type=image but Image is nil", ErrInvalidContent, pi)
			}
		case PartAudio:
			if p.Audio == nil {
				return fmt.Errorf("%w: Parts[%d].Type=audio but Audio is nil", ErrInvalidContent, pi)
			}
		case PartFile:
			if p.File == nil {
				return fmt.Errorf("%w: Parts[%d].Type=file but File is nil", ErrInvalidContent, pi)
			}
		default:
			return fmt.Errorf("%w: Parts[%d].Type=%q is unknown", ErrInvalidContent, pi, p.Type)
		}
	}
	return nil
}

// findContextLeak walks the materialized request and reports the
// FIRST oversize raw payload it finds. The caller uses the (site,
// size) to publish the event. Returns ok=false when the request is
// clean.
//
// Order: messages → content text → multimodal parts → tool-call
// arguments. Text-mode content checks the message-level Text field;
// multimodal checks per-part Text + each part's DataURL; the tool-call
// pass checks each entry's raw `Args`.
//
// Scope: the byte heavy-content check governs OFFLOADABLE content —
// content the runtime is expected to have routed to an ArtifactStub
// instead of carrying inline:
//
//   - `RoleTool` message text (Content.Text + PartText) — tool / MCP
//     observations, which the ObservationRenderer offloads when heavy.
//   - Binary `DataURL` parts (Image / Audio / File) of ANY role —
//     auto-materialized to ArtifactRef above the threshold.
//   - `ToolCalls[].Args` — the tool-call ARGUMENTS the prompt builder
//     replays turn over turn and the provider drivers map onto the
//     wire. Machine-authored, tool-shaped, and offloadable through the
//     same ArtifactStub path a tool RESULT takes, so it sits on the
//     offloadable side of the line next to `RoleTool` text rather than
//     on the exempt conversation-text side. A tool call whose arguments
//     legitimately exceed the threshold is the same bug this check
//     names everywhere else: a producer that should have passed a
//     reference.
//
// Plain conversation text on `RoleSystem` / `RoleUser` / `RoleAssistant`
// messages (Content.Text and PartText, including an injected rolling
// summary) is EXEMPT: it is legitimate conversation context that is not
// offloadable to an ArtifactStub, and its size is governed by the
// token-window guard (`ErrContextWindowExceeded`), not this byte check.
//
// Note: Artifact-shaped parts are skipped — they're exactly the
// canonical form we expect. URL-shaped parts are skipped:
// they're remote references, not in-prompt bytes.
func findContextLeak(req CompleteRequest, threshold int) (site string, size int, ok bool) {
	for mi, m := range req.Messages {
		// The byte heavy-content check covers RoleTool message text (this
		// branch) and binary DataURL parts of ANY role (below). Tool- or
		// task-derived content that is rendered under a CONVERSATION role —
		// e.g. the legacy text-only-provider observation replay and the
		// background-task outcome rendering in the react planner's prompt
		// builder — is byte-exempt here. Those paths rely on dispatch-time
		// source projection (the runtime stubs heavy results before
		// rendering) as their offload guarantee, not on this edge backstop.
		offloadableText := m.Role == RoleTool
		// Text-mode content — only the offloadable (tool-result) class
		// is subject to the byte check; conversation text is governed by
		// the token-window guard.
		if offloadableText && m.Content.Text != nil && len(*m.Content.Text) >= threshold {
			return fmt.Sprintf("Messages[%d].Content.Text", mi), len(*m.Content.Text), true
		}
		// Multimodal parts
		for pi, p := range m.Content.Parts {
			switch p.Type {
			case PartText:
				if offloadableText && len(p.Text) >= threshold {
					return fmt.Sprintf("Messages[%d].Parts[%d].Text", mi, pi), len(p.Text), true
				}
			case PartImage:
				if p.Image != nil && len(p.Image.DataURL) >= threshold {
					return fmt.Sprintf("Messages[%d].Parts[%d].Image.DataURL", mi, pi), len(p.Image.DataURL), true
				}
			case PartAudio:
				if p.Audio != nil && len(p.Audio.DataURL) >= threshold {
					return fmt.Sprintf("Messages[%d].Parts[%d].Audio.DataURL", mi, pi), len(p.Audio.DataURL), true
				}
			case PartFile:
				if p.File != nil && len(p.File.DataURL) >= threshold {
					return fmt.Sprintf("Messages[%d].Parts[%d].File.DataURL", mi, pi), len(p.File.DataURL), true
				}
			}
		}
		// Tool-call arguments. This is the one field on the outbound
		// request that can carry offloadable content to a provider
		// without passing through Content: the prompt builder copies a
		// trajectory step's args into it on every replayed turn, and the
		// driver translators map it straight onto the provider's
		// tool_calls block. Not walking it would leave the runtime's one
		// arrival-side guarantee resting on the claim that nothing ever
		// puts heavy content there — which restates the invariant
		// instead of checking it.
		for ci, tc := range m.ToolCalls {
			if len(tc.Args) >= threshold {
				return fmt.Sprintf("Messages[%d].ToolCalls[%d].Args", mi, ci), len(tc.Args), true
			}
		}
	}
	return "", 0, false
}

// identityQuad reads the calling identity from ctx. Prefers a full
// Quadruple (RunID present) when available; falls back to Identity
// + empty RunID otherwise.
func identityQuad(ctx context.Context) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	id, _ := identity.From(ctx)
	return identity.Quadruple{Identity: id}
}

// emitCostRecorded publishes the `llm.cost.recorded` event after a
// driver-level completion returns successfully. Driver-neutral: the
// safety wrapper is the mandatory innermost band around every driver,
// so this fires for bifrost, the dev-posture mock, and any future
// provider without a per-driver emit.
//
// This is an OBSERVABILITY emit only — the governance accumulator does
// NOT subscribe against it. Cost accounting is in-band synchronous (the
// accumulator folds each call's cost, and every intermediate retry /
// downgrade attempt via the per-call attempt-cost tap, in its PostCall),
// so the next ceiling check sees the latest total without a bus-delivery
// race. The event drives dashboards, replay tooling, and the reopen
// read-back reconstruction; it is not the accounting path.
//
// Best-effort — never blocks the request path on the bus. A nil bus is
// a no-op. The emit fires even when `cost.TotalCost == 0` because some
// providers don't report cost at all (token usage is still recorded).
//
// The payload is `events.SafePayload`: cost figures and token counts
// are operator-visible, not secret-shaped.
func emitCostRecorded(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, cost Cost, usage Usage, contextWindow int) {
	if bus == nil {
		return
	}
	now := time.Now()
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort cost-telemetry emit — must not block the LLM call path.
		Type:       EventTypeCostRecorded,
		Identity:   id,
		OccurredAt: now,
		Payload: CostRecordedPayload{
			Identity:            id,
			Model:               model,
			Cost:                cost,
			Usage:               usage,
			ContextWindowTokens: contextWindow,
			OccurredAt:          now,
		},
	})
}

// emitContextLeak publishes the `llm.context_leak` event. Best-effort
// — never block the request path on the bus.
func emitContextLeak(ctx context.Context, bus events.EventBus, id identity.Quadruple, model, site string, size, threshold int) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort emit — never block the request path on the bus (see func doc).
		Type:       EventTypeContextLeak,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: ContextLeakPayload{
			Identity:   id,
			Model:      model,
			LeakSite:   site,
			SizeBytes:  int64(size),
			Threshold:  threshold,
			OccurredAt: time.Now(),
		},
	})
}

// emitContextWindowExceeded publishes the `llm.context_window_exceeded`
// event. Best-effort.
func emitContextWindowExceeded(ctx context.Context, bus events.EventBus, id identity.Quadruple, model string, estimated, windowCap int, reserve float64) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort emit — never block the request path on the bus (see func doc).
		Type:       EventTypeContextWindowExceeded,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: ContextWindowExceededPayload{
			Identity:             id,
			Model:                model,
			EstimatedTokens:      estimated,
			ContextWindowTokens:  windowCap,
			ContextWindowReserve: reserve,
			OccurredAt:           time.Now(),
		},
	})
}
