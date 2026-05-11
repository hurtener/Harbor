// Package mock is Harbor's test-grade LLM driver. It self-registers
// under `"mock"` via init() and is the default driver in
// `internal/llm` (DefaultDriver = "mock"). Production deployments
// configure Phase 33's `bifrost` driver explicitly.
//
// The driver supports:
//
//   - Text-only AND multimodal (text + image/audio/file parts)
//     round-trips. The returned content is a deterministic
//     synthesis of the input — useful for property tests that need
//     stable assertions.
//   - Streaming via `req.Stream` + `req.OnContent` / `req.OnReasoning`
//     callbacks. The mock chunks the synthetic response into N
//     small pieces (controllable via `Options.StreamChunks`),
//     invokes the callbacks for each chunk, and surfaces a final
//     `done=true` call.
//   - ctx cancellation. Streaming respects `ctx.Done()` between
//     chunks; non-streaming respects it before the synthesis step.
//   - Cost/Usage reporting (synthetic; lets governance tests run
//     without a real provider).
//
// Concurrent-reuse (D-025): the driver itself is stateless. The
// only mutable state is the `closed` atomic.Bool which guards Close
// idempotency. Concurrent Complete calls are safe by construction.
//
// Test-only behaviour hooks (NOT operator-facing):
//
//   - `Options.SyntheticContent` overrides the generated response
//     content. Useful for failure-mode tests.
//   - `Options.ForcedError` makes every Complete return that error.
//     Useful for ErrClientClosed / retry-path tests.
//   - `Options.SeenIdentity` is an optional sink channel that
//     receives the identity from every Complete call. Used by the
//     D-025 concurrent-reuse test to verify no context bleed.
package mock

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// Options carries test-only knobs that influence the mock's
// response. Construct via `New(opts)` for tests that need the
// hooks; registry-path construction passes a zero-value Options.
type Options struct {
	// SyntheticContent overrides the generated response. Empty
	// means "use the default synthesis (echo of the last user
	// message)."
	SyntheticContent string
	// ForcedError makes every Complete fail with this error.
	ForcedError error
	// StreamChunks is the number of chunks the streaming path
	// produces. Zero defaults to 4 (small enough for fast tests).
	StreamChunks int
	// SeenIdentity is an optional sink for the identity each
	// Complete observes. Buffered N=1 by callers that race on it.
	SeenIdentity chan<- identity.Quadruple
	// PreStreamDelay is a hook for the cancellation test —
	// when > 0, the streaming path waits this duration BETWEEN
	// chunks (honouring ctx.Done()). Lets the test cancel
	// mid-stream and observe clean abort.
	PreStreamDelay time.Duration
}

// Driver is the exported mock driver type so test code can build
// instances directly (without going through the registry) when it
// needs to inject test-only Options.
type Driver struct {
	opts   Options
	closed atomic.Bool
}

// New constructs a mock Driver with the supplied Options.
//
// For registry-path use, callers should NOT call New directly —
// `llm.Open(...)` with `cfg.Driver = "mock"` constructs a zero-Opts
// driver via the init() registration. Direct New is for tests that
// inject SeenIdentity / ForcedError / SyntheticContent.
func New(opts Options) *Driver {
	if opts.StreamChunks <= 0 {
		opts.StreamChunks = 4
	}
	return &Driver{opts: opts}
}

// init self-registers the mock driver under "mock".
//
// Production binaries blank-import this package via cmd/harbor
// (Phase 64+); tests blank-import as needed.
func init() {
	llm.Register("mock", func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return New(Options{}), nil
	})
}

// Complete is the Driver.Complete entry point. The safety pass has
// already run upstream; this method synthesises a response.
//
// Honors ctx cancellation between work units (Step 1: identity
// observation; Step 2: streaming chunks; Step 3: final assembly).
// A cancelled ctx returns ctx.Err() (typically context.Canceled or
// context.DeadlineExceeded) — NOT a wrapped error.
func (d *Driver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if d.opts.ForcedError != nil {
		return llm.CompleteResponse{}, d.opts.ForcedError
	}
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}

	id := identityQuad(ctx)
	if d.opts.SeenIdentity != nil {
		select {
		case d.opts.SeenIdentity <- id:
		default:
			// drop if test forgot to drain
		}
	}

	content := d.synthesise(req)

	if req.Stream {
		if err := d.streamChunks(ctx, content, req.OnContent, req.OnReasoning); err != nil {
			return llm.CompleteResponse{}, err
		}
	}

	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}

	return llm.CompleteResponse{
		Content: content,
		Cost: llm.Cost{
			InputTokensCost:  0.000001 * float64(estimateInputChars(req)),
			OutputTokensCost: 0.000002 * float64(len(content)),
			TotalCost:        0.000001*float64(estimateInputChars(req)) + 0.000002*float64(len(content)),
			Currency:         "USD",
		},
		Usage: llm.Usage{
			PromptTokens:     estimateInputChars(req) / 4,
			CompletionTokens: len(content) / 4,
			TotalTokens:      (estimateInputChars(req) + len(content)) / 4,
			LatencyMS:        1, // deterministic for tests
		},
	}, nil
}

// Close marks the driver closed. Streaming is synchronous on the
// caller's goroutine, so there are no goroutines to drain. Idempotent.
func (d *Driver) Close(_ context.Context) error {
	d.closed.CompareAndSwap(false, true)
	return nil
}

// synthesise produces the response content. The default synthesis
// echoes the LAST user message (text or first text-part), prefixed
// with `mock:` so tests can distinguish the response shape.
// `Options.SyntheticContent` overrides.
func (d *Driver) synthesise(req llm.CompleteRequest) string {
	if d.opts.SyntheticContent != "" {
		return d.opts.SyntheticContent
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		if m.Content.Text != nil {
			return "mock:" + *m.Content.Text
		}
		for _, p := range m.Content.Parts {
			if p.Type == llm.PartText {
				return "mock:" + p.Text
			}
		}
		// Multimodal-only user message — synthesise something
		// observable so tests assert response presence.
		return "mock:multimodal"
	}
	return "mock:empty"
}

// streamChunks splits content into Options.StreamChunks pieces and
// invokes the content callback for each, with ctx-cancellation
// honoured between chunks. The reasoning callback (when set) fires
// once with a short marker string so test paths that assert
// "reasoning fired" have an anchor.
//
// Per AGENTS.md §5 "goroutines started by long-lived components
// must be cancellable and joined on shutdown" — d.wg is incremented
// before any goroutine spawns; this method runs SYNCHRONOUSLY on
// the caller's goroutine, so no spawn occurs unless a future
// extension adds one.
func (d *Driver) streamChunks(ctx context.Context, content string, onContent, onReasoning func(string, bool)) error {
	chunks := d.opts.StreamChunks
	if chunks > len(content) {
		chunks = len(content)
	}
	if chunks <= 0 {
		chunks = 1
	}
	chunkSize := (len(content) + chunks - 1) / chunks
	if chunkSize == 0 {
		chunkSize = 1
	}

	if onReasoning != nil {
		// Fire once with a short marker; not all providers stream
		// reasoning, so this is best-effort observable.
		onReasoning("mock-thinking", false)
		onReasoning("", true)
	}

	for i := 0; i < len(content); i += chunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.opts.PreStreamDelay > 0 {
			// Wait honours ctx — test path uses this to cancel
			// mid-stream and observe clean abort.
			t := time.NewTimer(d.opts.PreStreamDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		end := i + chunkSize
		if end > len(content) {
			end = len(content)
		}
		if onContent != nil {
			onContent(content[i:end], false)
		}
		// Give other goroutines a chance to run; helps the
		// concurrency tests assert progress under -race.
		runtime.Gosched()
	}
	if onContent != nil {
		onContent("", true)
	}
	return nil
}

// estimateInputChars returns the total chars across user-side
// content in the request. Used for synthetic cost/usage.
func estimateInputChars(req llm.CompleteRequest) int {
	total := 0
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			total += len(*m.Content.Text)
		}
		for _, p := range m.Content.Parts {
			if p.Type == llm.PartText {
				total += len(p.Text)
			}
		}
	}
	return total
}

// identityQuad reads the calling identity from ctx. Same logic as
// `internal/llm.identityQuad` but inlined to keep the mock package
// dependency-light.
func identityQuad(ctx context.Context) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	id, _ := identity.From(ctx)
	return identity.Quadruple{Identity: id}
}

// Compile-time assertion: *Driver implements llm.Driver.
var _ llm.Driver = (*Driver)(nil)
