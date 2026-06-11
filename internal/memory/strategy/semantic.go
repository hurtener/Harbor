package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hurtener/Harbor/internal/embeddings"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// semanticMaxEntries bounds the per-identity vector index. The
// semantic mode keeps the most recent N embedded turns; older
// entries drop oldest-first. Brute-force cosine over the index is
// the deliberate V1 ranking — an ANN index is a later concern at a
// scale conversations don't reach.
const semanticMaxEntries = 256

// semanticExec is the retrieval wrapper for `RetrievalSemantic`. It
// composes AROUND a strategy executor: every strategy method
// delegates to the inner executor unchanged (the rolling summary /
// truncation window keeps its exact semantics) while `AddTurn`
// additionally embeds the turn and `SearchTurns` ranks the embedded
// turns by cosine similarity.
//
// Vector persistence: one identity-scoped StateStore record at
// `kindMemoryVectors` per `(tenant, user, session, run)` — the same
// persistence floor the strategy records use, so all memory drivers
// inherit it with conformance parity. Vectors are DERIVED data: a
// `Restore` rebuilds the strategy state but not the vector index
// (turns restored from a snapshot re-enter the index as new turns
// arrive), and changing the embedding model requires re-embedding —
// a dimension mismatch at search time fails loudly rather than
// silently comparing incompatible vectors.
//
// Concurrent reuse: per-key read-modify-write on the vector record
// is serialised by a per-key mutex (the truncation executor's
// pattern); the wrapper itself holds no per-call state.
type semanticExec struct {
	inner    StrategyExecutor
	embedder memory.Embedder
	state    state.StateStore
	topK     int

	// keys serialises the per-identity vector-record
	// read-modify-write. Internally synchronised.
	keys sync.Map // map[quadKey]*semanticKeyLock

	mu     sync.Mutex
	closed bool
}

type semanticKeyLock struct {
	mu sync.Mutex
}

// memoryVectorRecord is the JSON shape persisted at
// `kindMemoryVectors`.
type memoryVectorRecord struct {
	Entries []memoryVectorEntry `json:"entries"`
}

// memoryVectorEntry pairs one stored turn with its embedding
// vector.
type memoryVectorEntry struct {
	Turn   memory.ConversationTurn `json:"turn"`
	Vector []float32               `json:"vector"`
}

func newSemanticExec(inner StrategyExecutor, deps Deps) *semanticExec {
	topK := deps.RetrievalTopK
	if topK <= 0 {
		topK = memory.DefaultSemanticTopK
	}
	return &semanticExec{
		inner:    inner,
		embedder: deps.Embedder,
		state:    deps.State,
		topK:     topK,
	}
}

func (e *semanticExec) keyLock(id identity.Quadruple) *semanticKeyLock {
	k := quadKeyFor(id)
	if v, ok := e.keys.Load(k); ok {
		return v.(*semanticKeyLock) //nolint:errcheck // e.keys only ever stores *semanticKeyLock — the assertion cannot fail by construction.
	}
	actual, _ := e.keys.LoadOrStore(k, &semanticKeyLock{})
	return actual.(*semanticKeyLock) //nolint:errcheck // e.keys only ever stores *semanticKeyLock — the assertion cannot fail by construction.
}

// AddTurn embeds the turn FIRST (so an embedding failure surfaces
// before any state mutates — fail loudly, never index-drift), then
// delegates to the inner strategy, then appends the vector entry to
// the identity-scoped record.
func (e *semanticExec) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ectx, err := embedCtx(ctx, id)
	if err != nil {
		return err
	}
	text := turnText(turn)
	vecs, err := e.embedder.Embed(ectx, []string{text})
	if err != nil {
		return fmt.Errorf("memory/strategy/semantic: embed turn: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return fmt.Errorf("memory/strategy/semantic: embedder returned %d vectors for 1 text", len(vecs))
	}
	if err := e.inner.AddTurn(ctx, id, turn); err != nil {
		return err
	}

	kl := e.keyLock(id)
	kl.mu.Lock()
	defer kl.mu.Unlock()
	rec, err := loadVectorRecord(ctx, e.state, id)
	if err != nil {
		return err
	}
	rec.Entries = append(rec.Entries, memoryVectorEntry{Turn: turn, Vector: vecs[0]})
	if len(rec.Entries) > semanticMaxEntries {
		rec.Entries = rec.Entries[len(rec.Entries)-semanticMaxEntries:]
	}
	return persistVectorRecord(ctx, e.state, id, rec)
}

// SearchTurns embeds the query and ranks the stored entries by
// cosine similarity, identity-scoped. A dimension mismatch (model
// drift between indexing and querying) fails loudly with
// `embeddings.ErrDimensionMismatch` — re-embedding is the
// operator's migration, never a silent skip.
func (e *semanticExec) SearchTurns(ctx context.Context, id identity.Quadruple, query string, limit int) ([]memory.ScoredTurn, error) {
	if e.isClosed() {
		return nil, memory.ErrStoreClosed
	}
	if query == "" {
		return nil, fmt.Errorf("%w: query is empty", embeddings.ErrEmptyInput)
	}
	if limit <= 0 {
		limit = e.topK
	}
	ectx, err := embedCtx(ctx, id)
	if err != nil {
		return nil, err
	}
	vecs, err := e.embedder.Embed(ectx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("memory/strategy/semantic: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("memory/strategy/semantic: embedder returned %d vectors for 1 query", len(vecs))
	}
	qv := vecs[0]

	kl := e.keyLock(id)
	kl.mu.Lock()
	rec, err := loadVectorRecord(ctx, e.state, id)
	kl.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]memory.ScoredTurn, 0, len(rec.Entries))
	for i, entry := range rec.Entries {
		score, cerr := embeddings.Cosine(qv, entry.Vector)
		if cerr != nil {
			return nil, fmt.Errorf("memory/strategy/semantic: entry %d: %w (re-embed after an embedding-model change)", i, cerr)
		}
		out = append(out, memory.ScoredTurn{Turn: entry.Turn, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetLLMContext delegates to the inner strategy unchanged — the
// semantic mode COMPOSES with the strategy's patch, it never
// replaces it.
func (e *semanticExec) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	return e.inner.GetLLMContext(ctx, id)
}

// EstimateTokens delegates to the inner strategy.
func (e *semanticExec) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	return e.inner.EstimateTokens(ctx, id)
}

// Flush drops the inner strategy's state AND the vector index for
// `id`.
func (e *semanticExec) Flush(ctx context.Context, id identity.Quadruple) error {
	if err := e.inner.Flush(ctx, id); err != nil {
		return err
	}
	kl := e.keyLock(id)
	kl.mu.Lock()
	defer kl.mu.Unlock()
	if err := e.state.Delete(ctx, id, kindMemoryVectors); err != nil {
		return fmt.Errorf("memory/strategy/semantic: Flush delete vectors: %w", err)
	}
	return nil
}

// Health delegates to the inner strategy.
func (e *semanticExec) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	return e.inner.Health(ctx, id)
}

// Snapshot delegates to the inner strategy. Vectors are derived
// data and deliberately excluded from the portable snapshot (see
// the type godoc).
func (e *semanticExec) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	return e.inner.Snapshot(ctx, id)
}

// Restore delegates to the inner strategy. The vector index is NOT
// reconstructed — restored turns re-enter the index as new turns
// arrive (vectors are derived, not source-of-truth).
func (e *semanticExec) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	return e.inner.Restore(ctx, id, snap)
}

// Close tears down the inner executor. Idempotent.
func (e *semanticExec) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return e.inner.Close(ctx)
}

func (e *semanticExec) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// embedCtx stamps the store-validated identity quadruple onto the
// context the embedder sees. Memory carries identity as an explicit
// argument (driver-validated at the boundary); the embedding edge
// is identity-in-ctx (fail-closed). The bridge keeps the embed
// call's billing/audit attribution pinned to the identity the
// vectors are scoped under, even when the incoming ctx carries a
// different (or no) identity.
func embedCtx(ctx context.Context, id identity.Quadruple) (context.Context, error) {
	if id.RunID != "" {
		ectx, err := identity.WithRun(ctx, id.Identity, id.RunID)
		if err != nil {
			return nil, fmt.Errorf("memory/strategy/semantic: identity: %w", err)
		}
		return ectx, nil
	}
	ectx, err := identity.With(ctx, id.Identity)
	if err != nil {
		return nil, fmt.Errorf("memory/strategy/semantic: identity: %w", err)
	}
	return ectx, nil
}

// turnText is the embedded representation of a turn: both sides of
// the exchange, so a query matches whichever side carried the
// signal.
func turnText(t memory.ConversationTurn) string {
	switch {
	case t.UserMessage == "":
		return t.AssistantResponse
	case t.AssistantResponse == "":
		return t.UserMessage
	default:
		return t.UserMessage + "\n" + t.AssistantResponse
	}
}

// loadVectorRecord reads + unmarshals the vector record for `id`.
// Missing record → empty record (first touch), never an error.
func loadVectorRecord(ctx context.Context, st state.StateStore, id identity.Quadruple) (memoryVectorRecord, error) {
	rec, err := st.Load(ctx, id, kindMemoryVectors)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return memoryVectorRecord{}, nil
		}
		return memoryVectorRecord{}, fmt.Errorf("memory/strategy/semantic: load vectors: %w", err)
	}
	var out memoryVectorRecord
	if err := json.Unmarshal(rec.Bytes, &out); err != nil {
		return memoryVectorRecord{}, fmt.Errorf("memory/strategy/semantic: unmarshal vectors: %w", err)
	}
	return out, nil
}

// persistVectorRecord marshals + writes the vector record through
// the injected StateStore.
func persistVectorRecord(ctx context.Context, st state.StateStore, id identity.Quadruple, rec memoryVectorRecord) error {
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory/strategy/semantic: marshal vectors: %w", err)
	}
	sr := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: id,
		Kind:     kindMemoryVectors,
		Bytes:    bytes,
	}
	if err := st.Save(ctx, sr); err != nil {
		return fmt.Errorf("memory/strategy/semantic: save vectors: %w", err)
	}
	return nil
}

// Compile-time assertion that *semanticExec satisfies
// StrategyExecutor.
var _ StrategyExecutor = (*semanticExec)(nil)
