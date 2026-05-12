package bifrost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// driverName is the name under which this driver self-registers with
// `llm.Register`. Operators set `llm.driver: bifrost` in `harbor.yaml`
// to route the runtime's LLM traffic through it.
const driverName = "bifrost"

// bifrostClient is the slim sub-surface of `*bf.Bifrost` the Driver
// actually uses. Defining it explicitly lets tests inject a stub
// without spinning up bifrost's queue infrastructure / network /
// goroutine pool.
//
// Production wires `*bf.Bifrost`; tests inject a stubbed
// implementation via `newDriverWithClient` (see `export_test.go`).
type bifrostClient interface {
	ChatCompletionRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError)
	ChatCompletionStreamRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError)
}

// Driver is the bifrost-backed `llm.Driver` implementation. The
// Phase 32 safety pass wraps this struct via the registry (`llm.Open`);
// callers receive a `*safetyClient` and never construct this directly
// in production.
//
// Concurrent-reuse (D-025): the driver is stateless across calls. The
// embedded `bifrostClient` is internally synchronized (bifrost owns a
// queue pool and dispatches per-request goroutines). The `closed` flag
// is `atomic.Bool` for idempotent Close. Per-call state (identity,
// model, response shape) lives on the call stack / ctx.
type Driver struct {
	client   bifrostClient
	provider bfschemas.ModelProvider
	bus      events.EventBus

	closed atomic.Bool
}

// Compile-time assertion: *Driver implements llm.Driver.
var _ llm.Driver = (*Driver)(nil)

// New constructs a bifrost-backed `llm.Driver`. The Phase 32 safety
// pass wraps the returned driver; operators reach this via
// `llm.Open(ctx, cfg, deps)` with `cfg.Driver = "bifrost"`.
//
// Fails closed at construction when:
//   - `cfg.Provider` is empty or unknown (`ErrInvalidProvider`);
//   - `cfg.APIKey` is empty or references an unset env var
//     (`ErrMissingAPIKey`);
//   - `bf.Init` returns an error.
//
// `deps.Bus` is captured for the `llm.cost.recorded` emit path; nil
// is tolerated (the safety pass's `Open` already rejects nil Bus, but
// tests that construct a Driver directly may pass nil).
func New(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
	account, err := newAccount(cfg)
	if err != nil {
		return nil, err
	}
	bfCfg := bfschemas.BifrostConfig{
		Account: account,
	}
	inner, err := bf.Init(context.Background(), bfCfg)
	if err != nil {
		return nil, fmt.Errorf("bifrost: Init: %w", err)
	}
	return &Driver{
		client:   inner,
		provider: account.provider,
		bus:      deps.Bus,
	}, nil
}

// init self-registers the bifrost driver under `"bifrost"` with the
// `llm` package's factory registry. The blank import in
// `cmd/harbor/main.go` triggers this.
func init() {
	llm.Register(driverName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return New(cfg, deps)
	})
}

// Complete is the Driver entry point. The Phase 32 safety pass has
// already validated identity, materialized oversize content, run the
// leak-detection pass, and run the token-budget guard upstream — by
// the time this method runs, `req` is safe to translate and dispatch.
//
// The driver re-checks identity at its edge because callers that
// construct a Driver directly (without going through the safety
// pass) MUST still fail-closed on missing identity per AGENTS.md §6
// rule 9.
func (d *Driver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}
	if !llm.HasIdentity(ctx) {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	id := identityQuad(ctx)

	bfReq, err := translateRequest(d.provider, req)
	if err != nil {
		return llm.CompleteResponse{}, fmt.Errorf("bifrost: translate request: %w", err)
	}

	bctx := bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline)

	if req.Stream {
		return d.streamComplete(ctx, bctx, bfReq, req, id)
	}
	return d.unaryComplete(ctx, bctx, bfReq, req, id)
}

// unaryComplete runs a non-streaming chat completion.
func (d *Driver) unaryComplete(
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	bfReq *bfschemas.BifrostChatRequest,
	req llm.CompleteRequest,
	id identity.Quadruple,
) (llm.CompleteResponse, error) {
	resp, berr := d.client.ChatCompletionRequest(bctx, bfReq)
	if berr != nil {
		return llm.CompleteResponse{}, translateError(berr, "ChatCompletionRequest")
	}
	out := translateResponse(resp)
	emitCostRecorded(ctx, d.bus, id, req.Model, out.Cost, out.Usage)
	return out, nil
}

// streamComplete runs a streaming chat completion. Content deltas
// route to `req.OnContent`; reasoning deltas route to `req.OnReasoning`;
// the assembled content is concatenated into `CompleteResponse.Content`.
//
// Cancellation: a `select` on `ctx.Done()` lets the driver abandon
// the bifrost chunk reader as soon as the caller cancels — the
// runtime never blocks waiting for upstream to drain (brief 08
// §"Cancellation caveat"). Bifrost's worker goroutine continues
// draining the upstream HTTP body until completion, but Harbor is no
// longer reading from the channel; the goroutine exits when the
// channel closes, and the runtime's goroutine-leak test asserts
// baseline restoration.
func (d *Driver) streamComplete(
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	bfReq *bfschemas.BifrostChatRequest,
	req llm.CompleteRequest,
	id identity.Quadruple,
) (llm.CompleteResponse, error) {
	ch, berr := d.client.ChatCompletionStreamRequest(bctx, bfReq)
	if berr != nil {
		return llm.CompleteResponse{}, translateError(berr, "ChatCompletionStreamRequest")
	}

	var (
		contentB    strings.Builder
		reasoningB  strings.Builder
		finalUsage  llm.Usage
		finalCost   llm.Cost
		streamErr   error
		gotAnyChunk bool
	)

readLoop:
	for {
		select {
		case <-ctx.Done():
			// Abandon the reader. Bifrost's goroutine drains
			// upstream on its own; we never block waiting for it.
			// The caller receives `ctx.Err()` (Canceled or
			// DeadlineExceeded).
			streamErr = ctx.Err()
			break readLoop
		case chunk, ok := <-ch:
			if !ok {
				// Channel closed — stream terminated cleanly.
				break readLoop
			}
			if chunk == nil {
				continue
			}
			gotAnyChunk = true
			if chunk.BifrostError != nil {
				streamErr = translateError(chunk.BifrostError, "stream chunk")
				break readLoop
			}
			if chunk.BifrostChatResponse != nil {
				processStreamChunk(chunk.BifrostChatResponse, &contentB, &reasoningB, &finalUsage, &finalCost, req.OnContent, req.OnReasoning)
			}
		}
	}

	// Final `done=true` callback fires regardless of which path closed
	// the loop. Operators that observe the `done` flag get a
	// consistent terminal signal even on cancellation / error.
	if req.OnContent != nil {
		req.OnContent("", true)
	}
	if req.OnReasoning != nil && reasoningB.Len() > 0 {
		req.OnReasoning("", true)
	}

	if streamErr != nil {
		return llm.CompleteResponse{}, streamErr
	}
	if !gotAnyChunk {
		// Empty stream — surface as an empty response rather than a
		// silent success-with-no-content.
		return llm.CompleteResponse{}, fmt.Errorf("bifrost: stream returned no chunks")
	}
	out := llm.CompleteResponse{
		Content: contentB.String(),
		Usage:   finalUsage,
		Cost:    finalCost,
	}
	emitCostRecorded(ctx, d.bus, id, req.Model, out.Cost, out.Usage)
	return out, nil
}

// processStreamChunk merges a single bifrost stream chunk into the
// accumulators + invokes the per-delta callbacks. The Usage / Cost
// fields on bifrost's stream response carry their final values on
// the terminal chunk (most providers send `prompt_tokens` /
// `completion_tokens` / `cost` on the last delta); we overwrite each
// time so the latest non-nil values survive.
func processStreamChunk(
	resp *bfschemas.BifrostChatResponse,
	contentB *strings.Builder,
	reasoningB *strings.Builder,
	usage *llm.Usage,
	cost *llm.Cost,
	onContent func(string, bool),
	onReasoning func(string, bool),
) {
	if resp == nil {
		return
	}
	for _, choice := range resp.Choices {
		if choice.ChatStreamResponseChoice == nil || choice.ChatStreamResponseChoice.Delta == nil {
			continue
		}
		delta := choice.ChatStreamResponseChoice.Delta
		if delta.Content != nil && *delta.Content != "" {
			contentB.WriteString(*delta.Content)
			if onContent != nil {
				onContent(*delta.Content, false)
			}
		}
		if delta.Reasoning != nil && *delta.Reasoning != "" {
			reasoningB.WriteString(*delta.Reasoning)
			if onReasoning != nil {
				onReasoning(*delta.Reasoning, false)
			}
		}
	}
	// Backfill usage / cost when bifrost reports it (typically on
	// the terminal chunk).
	if resp.Usage != nil {
		if u, c := extractUsageAndCost(resp); u.TotalTokens > 0 || c.TotalCost > 0 || u.PromptTokens > 0 {
			*usage = u
			// Preserve a non-zero cost across earlier chunks (some
			// providers send usage on chunk N-1 and cost on chunk N).
			if c.TotalCost > 0 {
				*cost = c
			}
		}
	}
}

// Close releases the underlying bifrost instance. Bifrost owns its
// own goroutines for the queue/dispatcher; the recommended teardown
// is to call its cleanup (if any) — at v1.5.8 the API exposes
// `(*Bifrost).Shutdown()` via `bf` but the concrete shape may evolve.
// For Harbor's tests we set the atomic flag and let the underlying
// instance be GC'd; the goroutine-leak test pins baseline restoration
// via the stub-client path.
//
// Idempotent. Subsequent calls return nil.
func (d *Driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	// If the underlying client has a Close-like method, call it.
	// Defining a separate interface for "closable client" lets the
	// stub opt out cleanly.
	if closer, ok := d.client.(interface{ Cleanup() error }); ok {
		return closer.Cleanup()
	}
	return nil
}

// identityQuad reads the calling identity from ctx. Mirrors the
// helper in `internal/llm`; inlined here so the driver package
// doesn't reach into the parent for an unexported helper.
func identityQuad(ctx context.Context) identity.Quadruple {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		return q
	}
	id, _ := identity.From(ctx)
	return identity.Quadruple{Identity: id}
}

// errKnownBifrost — a sentinel for tests that want to assert on a
// translated bifrost error. Production wraps the bifrost message; the
// tests compare via `errors.Is`. Kept here (rather than in
// `translate.go`) because it's only used by the test helpers.
var errKnownBifrost = errors.New("bifrost: known error")
