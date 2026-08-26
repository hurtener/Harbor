package bifrost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/artifacts"
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
// The file methods serve the provider-native upload pass (the
// `provider_native` attachment disposition, RFC §6.5): bifrost
// providers without file support return an `unsupported_operation`
// error from these methods, which the upload pass degrades loudly to
// the `ArtifactStub` rendering.
//
// Production wires `*bf.Bifrost`; tests inject a stubbed
// implementation via `newDriverWithClient` (see `export_test.go`).
type bifrostClient interface {
	ChatCompletionRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError)
	ChatCompletionStreamRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError)
	FileUploadRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError)
	FileDeleteRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostFileDeleteRequest) (*bfschemas.BifrostFileDeleteResponse, *bfschemas.BifrostError)
}

// Driver is the bifrost-backed `llm.Driver` implementation. The
// safety pass wraps this struct via the registry (`llm.Open`);
// callers receive a `*safetyClient` and never construct this directly
// in production.
//
// Concurrent-reuse: the driver is stateless across calls
// except for the provider-file cache, which is internally
// synchronized (`providerFileCache` guards its map + LRU list with a
// mutex and is documented as such). The embedded `bifrostClient` is
// internally synchronized (bifrost owns a queue pool and dispatches
// per-request goroutines). The `closed` flag is `atomic.Bool` for
// idempotent Close. Per-call state (identity, model, response shape)
// lives on the call stack / ctx.
type Driver struct {
	client   bifrostClient
	provider bfschemas.ModelProvider
	bus      events.EventBus
	// artifacts is the runtime artifact store the provider-native
	// upload pass reads attachment bytes from (read-only after
	// construction; the store itself is internally synchronized).
	artifacts artifacts.ArtifactStore
	// files is the identity-scoped provider file_id cache — the
	// driver-owned lifecycle (TTL + LRU evict with remote delete, plus
	// a Close-time sweep). Internally synchronized.
	files         *providerFileCache
	account       *Account
	providerRoute llm.ProviderRouteConfig
	routeClient   bifrostClient
	routePool     *routeClientPool

	closed atomic.Bool
}

// Compile-time assertion: *Driver implements llm.Driver.
var _ llm.Driver = (*Driver)(nil)
var _ llm.ProviderRouteSelectionValidator = (*Driver)(nil)

// ValidateProviderRouteSelection rejects unsupported provider/endpoint shapes
// before governance and model-sensitive wrappers observe the selected model.
func (d *Driver) ValidateProviderRouteSelection(selected llm.SelectedProviderRoute) error {
	provider := bfschemas.ModelProvider(selected.Provider)
	if !curatedRouteProvider(provider) {
		return llm.ErrProviderRouteInvalid
	}
	return validateCuratedRouteEndpoint(provider, selected.Endpoint)
}

// New constructs a bifrost-backed `llm.Driver`. The safety
// pass wraps the returned driver; operators reach this via
// `llm.Open(ctx, cfg, deps)` with `cfg.Driver = "bifrost"`.
//
// Fails closed at construction when:
//   - `cfg.Provider` is empty or unknown (`ErrInvalidProvider`);
//   - `cfg.APIKey` is empty or references an unset env var
//     (`ErrMissingAPIKey`);
//   - `bf.Init` returns an error.
//
// `deps.Bus` is captured for the provider-file-uploaded observability
// emit (usage/cost telemetry rides the driver-neutral safety-wrapper
// band, not this driver). Nil is tolerated (the safety pass's `Open`
// already rejects nil Bus, but tests that construct a Driver directly
// may pass nil).
func New(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
	account, err := newAccount(cfg, deps)
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
	driver := &Driver{
		client:        inner,
		provider:      account.provider,
		bus:           deps.Bus,
		artifacts:     deps.Artifacts,
		files:         newProviderFileCache(defaultFileCacheCapacity, defaultFileCacheTTL),
		account:       account,
		providerRoute: deps.ProviderRoute,
	}
	if deps.ProviderRoute.Resolver != nil {
		routeAccount, routeErr := newRouteAccount(cfg.NetworkDefaults)
		if routeErr != nil {
			inner.Shutdown()
			return nil, fmt.Errorf("bifrost: initialize curated provider routes: %w", routeErr)
		}
		routeInner, routeErr := bf.Init(context.Background(), bfschemas.BifrostConfig{Account: routeAccount})
		if routeErr != nil {
			inner.Shutdown()
			return nil, fmt.Errorf("bifrost: initialize curated provider route workers: %w", routeErr)
		}
		driver.routeClient = routeInner
		driver.routePool = newRouteClientPool(defaultRouteClientPoolCapacity, cfg.NetworkDefaults)
	}
	return driver, nil
}

// init self-registers the bifrost driver under `"bifrost"` with the
// `llm` package's factory registry. The blank import in
// `cmd/harbor/main.go` triggers this.
func init() {
	llm.Register(driverName, New)
}

// Complete is the Driver entry point. The safety pass has
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
	providerID := d.provider
	callClient := d.client
	var releaseRouteClient func()
	if trusted, explicit := llm.TrustedProviderRouteFrom(ctx); explicit {
		selected, selectedOK := llm.SelectedProviderRouteFrom(ctx)
		if err := llm.ValidateProviderRoute(trusted.Route); err != nil {
			return llm.CompleteResponse{}, err
		}
		if requestHasProviderNativeWork(req) {
			return llm.CompleteResponse{}, fmt.Errorf("bifrost: provider-native uploads are unavailable for an external provider route")
		}
		if d.providerRoute.Resolver == nil {
			return llm.CompleteResponse{}, llm.ErrProviderRouteResolverUnavailable
		}
		if !selectedOK || !selected.ExpiresAt.After(time.Now()) || trusted.RuntimeID == "" || trusted.RuntimeID != d.providerRoute.RuntimeID || trusted.EffectiveAgentID == "" || trusted.TaskID == "" || trusted.Purpose != llm.ProviderRoutePurposeRun || req.Model != selected.Model {
			return llm.CompleteResponse{}, llm.ErrProviderRouteInvalid
		}
		var scope *llm.AttemptScope
		var err error
		ctx, scope, err = llm.EnsureAttemptScope(ctx)
		if err != nil {
			return llm.CompleteResponse{}, err
		}
		resolved, err := llm.ResolveProviderRoute(ctx, d.providerRoute, llm.ProviderRouteRequest{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, LogicalRunID: id.RunID,
			EffectiveAgentID: trusted.EffectiveAgentID, RuntimeID: trusted.RuntimeID, TaskID: trusted.TaskID,
			LogicalCallID: scope.LogicalCallID, RouteID: trusted.Route.RouteID,
			RouteGeneration: trusted.Route.RouteGeneration, ProviderConnectionID: trusted.Route.ProviderConnectionID,
			ProviderConnectionGeneration: trusted.Route.ProviderConnectionGeneration,
			CredentialAssetGeneration:    trusted.Route.CredentialAssetGeneration, ModelSelector: trusted.Route.ModelSelector,
			Purpose: trusted.Purpose,
		}, time.Now())
		if err != nil {
			return llm.CompleteResponse{}, err
		}
		if !llm.ProviderRouteResolutionMatchesSelection(resolved, selected) {
			return llm.CompleteResponse{}, llm.ErrProviderRouteInvalid
		}
		providerID = bfschemas.ModelProvider(resolved.Provider)
		if !curatedRouteProvider(providerID) || d.routeClient == nil {
			return llm.CompleteResponse{}, llm.ErrProviderRouteInvalid
		}
		ctx = llm.WithResolvedProviderRoute(ctx, resolved)
		callClient = d.routeClient
		if resolved.Endpoint != nil && resolved.Endpoint.Kind == llm.ProviderEndpointOpenAICompatible {
			if d.routePool == nil {
				return llm.CompleteResponse{}, llm.ErrProviderRouteInvalid
			}
			callClient, releaseRouteClient, err = d.routePool.acquire(ctx, routeClientPoolKey{
				TenantID: id.TenantID, RuntimeID: trusted.RuntimeID, RouteID: resolved.RouteID,
				RouteGeneration: resolved.RouteGeneration, ProviderConnectionID: resolved.ProviderConnectionID,
				ProviderConnectionGeneration: resolved.ProviderConnectionGeneration,
				CredentialAssetGeneration:    resolved.CredentialAssetGeneration,
				Provider:                     resolved.Provider, EndpointDigest: resolved.Endpoint.Digest,
			}, resolved.Endpoint.Value)
			if err != nil {
				return llm.CompleteResponse{}, err
			}
			defer releaseRouteClient()
		}
	}

	bctx := bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline)

	// Provider-native upload pass: any `ProviderNative`-flagged part
	// without a `ProviderFileID` is uploaded to the provider's file
	// surface (cache-aware, identity-scoped) and rewritten to the
	// opaque reference BEFORE translation. Copy-on-write — the
	// caller's request value is never mutated.
	var err error
	req, err = d.applyProviderNative(ctx, bctx, req, id)
	if err != nil {
		return llm.CompleteResponse{}, err
	}

	bfReq, err := translateRequest(providerID, req)
	if err != nil {
		return llm.CompleteResponse{}, fmt.Errorf("bifrost: translate request: %w", err)
	}

	if req.Stream {
		return d.streamComplete(callClient, ctx, bctx, bfReq, req)
	}
	return d.unaryComplete(callClient, ctx, bctx, bfReq)
}

// unaryComplete runs a non-streaming chat completion.
func (d *Driver) unaryComplete(
	client bifrostClient,
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	bfReq *bfschemas.BifrostChatRequest,
) (llm.CompleteResponse, error) {
	resp, berr := client.ChatCompletionRequest(bctx, bfReq)
	if berr != nil {
		return llm.CompleteResponse{}, translateErrorForContext(ctx, berr, "ChatCompletionRequest")
	}
	out := translateResponse(resp)
	return out, nil
}

// streamComplete runs a streaming chat completion. Content deltas
// route to `req.OnContent`; reasoning deltas route to `req.OnReasoning`;
// the assembled content is concatenated into `CompleteResponse.Content`.
//
// Cancellation: a `select` on `ctx.Done()` lets the driver abandon
// the bifrost chunk reader as soon as the caller cancels — the
// runtime never blocks waiting for upstream to drain (a design premise:
// §"Cancellation caveat"). Bifrost's worker goroutine continues
// draining the upstream HTTP body until completion, but Harbor is no
// longer reading from the channel; the goroutine exits when the
// channel closes, and the runtime's goroutine-leak test asserts
// baseline restoration.
func (d *Driver) streamComplete(
	client bifrostClient,
	ctx context.Context,
	bctx *bfschemas.BifrostContext,
	bfReq *bfschemas.BifrostChatRequest,
	req llm.CompleteRequest,
) (llm.CompleteResponse, error) {
	ch, berr := client.ChatCompletionStreamRequest(bctx, bfReq)
	if berr != nil {
		return llm.CompleteResponse{}, translateErrorForContext(ctx, berr, "ChatCompletionStreamRequest")
	}

	var (
		contentB       strings.Builder
		reasoning      = newReasoningAccumulator()
		finalToolCalls []llm.ToolCallStructured
		finalUsage     llm.Usage
		finalCost      llm.Cost
		streamErr      error
		gotAnyChunk    bool
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
				streamErr = translateErrorForContext(ctx, chunk.BifrostError, "stream chunk")
				break readLoop
			}
			if chunk.BifrostChatResponse != nil {
				processStreamChunk(chunk.BifrostChatResponse, &contentB, reasoning, &finalToolCalls, &finalUsage, &finalCost, req.OnContent, req.OnReasoning)
			}
		}
	}

	// Final `done=true` callback fires regardless of which path closed
	// the loop. Operators that observe the `done` flag get a
	// consistent terminal signal even on cancellation / error.
	if req.OnContent != nil {
		req.OnContent("", true)
	}
	if req.OnReasoning != nil && reasoning.rawObserved {
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
		Content:   contentB.String(),
		ToolCalls: finalToolCalls,
		Reasoning: reasoning.result(),
		Usage:     finalUsage,
		Cost:      finalCost,
	}
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
	reasoning *reasoningAccumulator,
	toolCalls *[]llm.ToolCallStructured,
	usage *llm.Usage,
	cost *llm.Cost,
	onContent func(string, bool),
	onReasoning func(string, bool),
) {
	if resp == nil {
		return
	}
	for _, choice := range resp.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.ChatStreamResponseChoice == nil || choice.Delta == nil {
			continue
		}
		delta := choice.Delta
		if delta.Content != nil && *delta.Content != "" {
			contentB.WriteString(*delta.Content)
			if onContent != nil {
				onContent(*delta.Content, false)
			}
		}
		if delta.Reasoning != nil {
			reasoning.observeRaw(*delta.Reasoning)
			if onReasoning != nil {
				onReasoning(*delta.Reasoning, false)
			}
		}
		// Details are the fallback only when this completion never
		// exposes raw reasoning. The accumulator still records them so
		// details-only providers retain their trace.
		reasoning.observeDetails(delta.ReasoningDetails)
		// accumulate streamed tool-call deltas.
		// Per the OpenAI streaming spec (also followed by Anthropic via
		// Bedrock, Gemini's OpenAI-compat surface, and OpenRouter): the
		// FIRST delta for a tool call carries `id + name`; subsequent
		// deltas carry empty id + null name + an args FRAGMENT to be
		// concatenated onto the prior args. The `index` field is the
		// load-bearing discriminator — it's stable across all fragments
		// of the same tool call. Without index-keyed merge, providers
		// that stream args incrementally (Bedrock streams ~1-byte
		// fragments) produce N broken half-built ToolCalls; the
		// trajectory replay then sends a bogus assistant turn to the
		// next request and the LLM gets stuck in a repair loop.
		for _, tc := range delta.ToolCalls {
			var args json.RawMessage
			if tc.Function.Arguments != "" {
				args = json.RawMessage(tc.Function.Arguments)
			}
			callID := ""
			if tc.ID != nil {
				callID = *tc.ID
			}
			name := ""
			if tc.Function.Name != nil {
				name = *tc.Function.Name
			}
			mergeStreamedToolCall(toolCalls, llm.ToolCallStructured{
				ID:    callID,
				Name:  name,
				Args:  args,
				Index: tc.Index,
			})
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
// own goroutines for the queue/dispatcher — `(*Bifrost).Shutdown()`
// joins the per-provider worker pools. Previously this method probed
// only for a `Cleanup() error` shape, which `*bf.Bifrost` does NOT
// expose — a production stack Close therefore leaked bifrost's entire
// worker pool (~1000 goroutines per provider). Found by the
// composed E2E's goroutine-baseline assertion
// (test/integration/wavec_test.go) and fixed per CLAUDE.md §17.6
// (fix what the integration test finds, wherever the bug lives). Both
// shapes are probed so a bifrost version that grows an
// error-returning teardown is still honoured; the stub client
// implements neither and opts out cleanly.
//
// Close also drains the provider file_id cache and best-effort
// deletes every cached remote file (`FileDeleteRequest`) so headless
// consumers that never run a dev loop don't leak provider-side files
// — the driver owns the full file lifecycle (RFC §6.5). Delete
// failures are logged at Warn and never block teardown.
//
// Idempotent. Subsequent calls return nil.
func (d *Driver) Close(ctx context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	d.cleanupProviderFiles(ctx)
	if d.routePool != nil {
		d.routePool.Close()
	}
	if c, ok := d.routeClient.(interface{ Shutdown() }); ok {
		c.Shutdown()
	}
	switch c := d.client.(type) {
	case interface{ Cleanup() error }:
		return c.Cleanup()
	case interface{ Shutdown() }:
		// The v1.5.x `*bf.Bifrost` teardown: joins the provider
		// worker pools; returns nothing.
		c.Shutdown()
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

// mergeStreamedToolCall merges one streamed tool-call delta into the
// accumulator (step 10/11 audit revision).
//
// Per the OpenAI streaming spec, tool-call deltas use `index` as the
// stable per-tool-call discriminator across SSE chunks. The first
// delta for a given index carries `id + name`; subsequent deltas for
// the SAME index carry empty id, null name, and an args FRAGMENT to
// be appended (string concatenation) onto the prior args. Bedrock
// (Anthropic via OpenRouter) streams args in 1-byte-ish fragments,
// so without correct incremental merge the LLM thread fills with
// half-built ToolCalls.
//
// Merge rules:
//   - Look up by Index FIRST (always present on streaming deltas).
//   - If the existing entry has an empty `ID` and the delta carries
//     one, adopt the delta's ID (the first non-empty wins).
//   - Same for `Name`: keep the first non-empty (subsequent deltas
//     report Name: null).
//   - For `Args`: concatenate fragments. Empty fragments are no-ops.
//
// Fallback (no Index found AND no matching ID): append. This handles
// pre-streaming providers + the unary path's full-shape entries.
func mergeStreamedToolCall(acc *[]llm.ToolCallStructured, delta llm.ToolCallStructured) {
	// Index-keyed merge for streaming deltas.
	for i, existing := range *acc {
		if existing.Index == delta.Index && (existing.ID != "" || delta.ID != "" || existing.Name != "" || delta.Name != "" || len(existing.Args) > 0) {
			// Same-position match. Adopt first-non-empty ID + Name;
			// concatenate args fragments.
			if (*acc)[i].ID == "" && delta.ID != "" {
				(*acc)[i].ID = delta.ID
			}
			if (*acc)[i].Name == "" && delta.Name != "" {
				(*acc)[i].Name = delta.Name
			}
			if len(delta.Args) > 0 {
				if len(existing.Args) == 0 {
					(*acc)[i].Args = delta.Args
				} else {
					// Args are stringified JSON fragments to concatenate.
					(*acc)[i].Args = append([]byte{}, existing.Args...)
					(*acc)[i].Args = append((*acc)[i].Args, delta.Args...)
				}
			}
			return
		}
	}
	// Defensive ID-keyed fallback (unary path / provider that fills
	// `id` on every delta without using index).
	if delta.ID != "" {
		for i, existing := range *acc {
			if existing.ID == delta.ID {
				if delta.Name != "" {
					(*acc)[i].Name = delta.Name
				}
				if len(delta.Args) > 0 {
					(*acc)[i].Args = delta.Args
				}
				return
			}
		}
	}
	*acc = append(*acc, delta)
}
