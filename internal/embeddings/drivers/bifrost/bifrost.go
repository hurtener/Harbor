// Package bifrost is Harbor's production `embeddings.Embedder`
// driver, wired to the bifrost provider gateway's embedding surface.
// Operators select it with `embeddings.driver: bifrost` (the
// default); the embedding provider + model are configured separately
// from the chat model.
//
// The driver owns its own gateway instance: the embedding provider
// and the chat provider are independent operator choices, and an
// embeddings-only consumer must be able to construct this driver
// without any chat-client wiring.
//
// Identity-mandatory: the registry path (`embeddings.Open`) rejects
// missing-identity calls before the driver runs; the driver
// re-checks at its own edge so direct constructors fail closed too
// (AGENTS.md §6 rule 9).
//
// Concurrent reuse: the driver is stateless across calls. The
// embedded gateway client is internally synchronized; the `closed`
// flag is `atomic.Bool` for idempotent Close. Per-call state lives
// on the call stack / ctx.
package bifrost

import (
	"context"
	"fmt"
	"sync/atomic"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/embeddings"
)

// driverName is the name under which this driver self-registers with
// `embeddings.Register`.
const driverName = "bifrost"

// embeddingClient is the slim sub-surface of `*bf.Bifrost` the
// Driver uses. Defining it explicitly lets tests inject a stub
// without spinning up bifrost's queue infrastructure / network /
// goroutine pool. Production wires `*bf.Bifrost`.
type embeddingClient interface {
	EmbeddingRequest(ctx *bfschemas.BifrostContext, req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError)
}

// Driver is the bifrost-backed `embeddings.Embedder`. Construct via
// `embeddings.Open(ctx, cfg, deps)` with `cfg.Driver = "bifrost"`
// (or empty — bifrost is the default); tests may call `New`
// directly.
type Driver struct {
	client     embeddingClient
	provider   bfschemas.ModelProvider
	model      string
	dimensions int

	closed atomic.Bool
}

// Compile-time assertion: *Driver implements embeddings.Embedder.
var _ embeddings.Embedder = (*Driver)(nil)

// New constructs a bifrost-backed `embeddings.Embedder`.
//
// Fails closed at construction when:
//   - `cfg.Provider` is empty or not a known gateway provider
//     (`ErrInvalidProvider`);
//   - `cfg.Model` is empty (`ErrMissingModel`);
//   - `cfg.APIKey` is empty or references an unset env var
//     (`ErrMissingAPIKey`);
//   - the gateway init fails.
func New(cfg embeddings.ConfigSnapshot, _ embeddings.Deps) (embeddings.Embedder, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: embeddings model is empty (set embeddings.model)", ErrMissingModel)
	}
	account, err := newAccount(cfg)
	if err != nil {
		return nil, err
	}
	inner, err := bf.Init(context.Background(), bfschemas.BifrostConfig{Account: account})
	if err != nil {
		return nil, fmt.Errorf("embeddings/bifrost: Init: %w", err)
	}
	return &Driver{
		client:     inner,
		provider:   account.provider,
		model:      cfg.Model,
		dimensions: cfg.Dimensions,
	}, nil
}

// init self-registers the driver under `"bifrost"` with the
// embeddings factory registry. The production driver aggregator
// (`internal/drivers/prod`) blank-imports this package so the
// registration fires at boot.
func init() {
	embeddings.Register(driverName, New)
}

// Embed implements embeddings.Embedder. The registry wrapper has
// already validated identity + input shape upstream; the driver
// re-checks identity because direct constructors MUST still fail
// closed on a missing identity.
func (d *Driver) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if d.closed.Load() {
		return nil, embeddings.ErrClientClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !embeddings.HasIdentity(ctx) {
		return nil, embeddings.ErrIdentityMissing
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w: no texts supplied", embeddings.ErrEmptyInput)
	}

	req := &bfschemas.BifrostEmbeddingRequest{
		Provider: d.provider,
		Model:    d.model,
		Input:    &bfschemas.EmbeddingInput{Texts: texts},
	}
	if d.dimensions > 0 {
		dims := d.dimensions
		req.Params = &bfschemas.EmbeddingParameters{Dimensions: &dims}
	}

	bctx := bfschemas.NewBifrostContext(ctx, bfschemas.NoDeadline)
	resp, berr := d.client.EmbeddingRequest(bctx, req)
	if berr != nil {
		return nil, translateError(berr, "EmbeddingRequest")
	}
	return translateResponse(resp, len(texts))
}

// Close releases the underlying gateway instance, joining its
// per-provider worker pools. Idempotent; subsequent calls return
// nil. Both teardown shapes are probed so a gateway version that
// grows an error-returning teardown is still honoured; the stub
// client implements neither and opts out cleanly.
func (d *Driver) Close(_ context.Context) error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	switch c := d.client.(type) {
	case interface{ Cleanup() error }:
		return c.Cleanup()
	case interface{ Shutdown() }:
		c.Shutdown()
	}
	return nil
}

// translateResponse converts the gateway embedding response into
// Harbor's `[][]float32`, ordered by the response rows' `Index`
// (providers may reorder). Every input must yield a float-array
// vector; any other encoding (base64 string, int8 / int32 packed
// formats) fails loudly — Harbor requests no alternative encoding,
// so receiving one is a provider contract violation, never a value
// to silently coerce.
func translateResponse(resp *bfschemas.BifrostEmbeddingResponse, want int) ([][]float32, error) {
	if resp == nil || len(resp.Data) == 0 {
		return nil, fmt.Errorf("embeddings/bifrost: provider returned no embedding rows")
	}
	if len(resp.Data) != want {
		return nil, fmt.Errorf("embeddings/bifrost: provider returned %d rows for %d inputs", len(resp.Data), want)
	}
	out := make([][]float32, want)
	for _, row := range resp.Data {
		if row.Index < 0 || row.Index >= want {
			return nil, fmt.Errorf("embeddings/bifrost: provider returned out-of-range index %d (inputs: %d)", row.Index, want)
		}
		if out[row.Index] != nil {
			return nil, fmt.Errorf("embeddings/bifrost: provider returned duplicate index %d", row.Index)
		}
		arr := row.Embedding.EmbeddingArray
		if arr == nil {
			return nil, fmt.Errorf("embeddings/bifrost: row %d carries no float-array embedding (unsupported encoding)", row.Index)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("embeddings/bifrost: row %d carries an empty embedding", row.Index)
		}
		vec := make([]float32, len(arr))
		for i, v := range arr {
			vec[i] = float32(v)
		}
		out[row.Index] = vec
	}
	for i, vec := range out {
		if vec == nil {
			return nil, fmt.Errorf("embeddings/bifrost: provider returned no row for input %d", i)
		}
	}
	return out, nil
}

// translateError converts a gateway error into a wrapped Go error.
func translateError(berr *bfschemas.BifrostError, kind string) error {
	if berr == nil {
		return nil
	}
	msg := ""
	if berr.Error != nil && berr.Error.Message != "" {
		msg = berr.Error.Message
	} else if berr.Type != nil {
		msg = *berr.Type
	}
	if berr.StatusCode != nil && *berr.StatusCode > 0 {
		return fmt.Errorf("embeddings/bifrost: %s: status %d: %s", kind, *berr.StatusCode, msg)
	}
	return fmt.Errorf("embeddings/bifrost: %s: %s", kind, msg)
}
