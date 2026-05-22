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
// invariants every `Complete` MUST respect (D-026 + D-021 + AGENTS.md
// §6 rule 9):
//
//  1. Identity is mandatory — missing → ErrIdentityMissing.
//  2. Auto-materialize oversize DataURL content to ArtifactRef
//     (D-022). Emits `llm.image.materialized`.
//  3. Assert no raw heavy content survived (D-026 / AGENTS.md §13).
//     Emits `llm.context_leak` on failure.
//  4. Estimate total tokens against ModelProfile.ContextWindowTokens
//     and fail with ErrContextWindowExceeded when within the
//     ContextWindowReserve margin (D-026). Emits
//     `llm.context_window_exceeded` on failure.
//
// The wrapper is mandatory by construction — `registry.Open` returns
// a `*safetyClient`, not a raw `Driver`. Drivers that need to test
// against a bare `Driver` can construct one in their own package
// tests, but production calls always route through here.
//
// Concurrent-reuse contract (D-025): the wrapper is stateless across
// calls. The `closed` flag is `atomic.Bool` for the idempotent Close
// path; `cfg` is read-only after construction.
type safetyClient struct {
	deps   Deps
	driver Driver
	cfg    ConfigSnapshot
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
	// ctx has no deadline, layer one in defensively. (Long streams
	// from real providers can hang; the bifrost driver lands its
	// own per-call timeout from cfg.Timeout in Phase 33 — this is
	// the universal floor.)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}
	return c.driver.Complete(ctx, materialized)
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
// Content; ErrInvalidConfig for empty Model.
func validateRequest(req CompleteRequest) error {
	if req.Model == "" {
		return fmt.Errorf("%w: CompleteRequest.Model is empty", ErrInvalidConfig)
	}
	for mi, m := range req.Messages {
		if err := validateContent(m.Content); err != nil {
			return fmt.Errorf("messages[%d]: %w", mi, err)
		}
	}
	return nil
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
// Order: messages → content text → multimodal parts. Text-mode
// content checks the message-level Text field; multimodal checks
// per-part Text + each part's DataURL.
//
// Note: Artifact-shaped parts are skipped — they're exactly the
// canonical form we expect (D-022). URL-shaped parts are skipped:
// they're remote references, not in-prompt bytes.
func findContextLeak(req CompleteRequest, threshold int) (site string, size int, ok bool) {
	for mi, m := range req.Messages {
		// Text-mode content
		if m.Content.Text != nil && len(*m.Content.Text) >= threshold {
			return fmt.Sprintf("Messages[%d].Content.Text", mi), len(*m.Content.Text), true
		}
		// Multimodal parts
		for pi, p := range m.Content.Parts {
			switch p.Type {
			case PartText:
				if len(p.Text) >= threshold {
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

// emitContextLeak publishes the `llm.context_leak` event. Best-effort
// — never block the request path on the bus.
func emitContextLeak(ctx context.Context, bus events.EventBus, id identity.Quadruple, model, site string, size, threshold int) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort event emit; publish failure must not fail the safety pass
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
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort event emit; publish failure must not fail the safety pass
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
