package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// GetDeps carries the dependencies Get composes over.
type GetDeps struct {
	// Store supplies the committed execution-memory projection.
	Store memory.MemoryStore
	// DriverName is the configured memory-driver name surfaced on the
	// returned row.
	DriverName string
	// HeavyThreshold is the inline-payload bound in bytes — the runtime
	// supplies the pinned Console inline-payload bound, NOT the
	// operator's LLM-context `artifacts.heavy_output_threshold_bytes`,
	// because the selected arm of this reply is Protocol-visible. A
	// value whose byte length meets or exceeds it routes through the
	// source-bound artifact read. A non-positive threshold fails loud (a zero
	// threshold would route every value).
	HeavyThreshold int
}

// Get answers the `memory.get` Protocol method: it resolves a single
// memory record by key within the caller's identity scope and returns
// the full detail — metadata + post-redaction value (below the
// heavy-content threshold) OR a `MemoryArtifactRef` (at or above it).
//
// Identity is mandatory. Heavy values return a source-bound reference resolved
// by artifacts.get against current memory, without making a separately retained
// artifact copy. EXACTLY ONE of Value / ValueArtifact is populated on success.
//
// A key that resolves to no record returns `memory.ErrNotFound` — the
// caller maps it onto `CodeNotFound`.
func Get(ctx context.Context, deps GetDeps, req prototypes.MemoryGetRequest, id identity.Quadruple) (prototypes.MemoryGetResponse, error) {
	if deps.Store == nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: Store is nil")
	}
	if deps.HeavyThreshold <= 0 {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: HeavyThreshold %d is non-positive", deps.HeavyThreshold)
	}
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryGetResponse{}, err
	}
	if req.Key == "" {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("%w: empty key", ErrInvalidFilter)
	}
	if err := ctx.Err(); err != nil {
		return prototypes.MemoryGetResponse{}, err
	}

	snap, err := deps.Store.Inspect(ctx, id)
	if err != nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: inspect: %w", err)
	}
	rows, err := snapshotTurns(snap, id, deps.DriverName, deps.HeavyThreshold)
	if err != nil {
		return prototypes.MemoryGetResponse{}, err
	}

	var target *projectedTurn
	for i := range rows {
		if rows[i].item.Key == req.Key {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: key %q: %w", req.Key, memory.ErrNotFound)
	}

	detail, err := buildDetail(deps, *target, id)
	if err != nil {
		return prototypes.MemoryGetResponse{}, err
	}
	return prototypes.MemoryGetResponse{
		Detail:          detail,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// buildDetail assembles the MemoryItemDetail for a resolved row,
// applying the heavy-content bypass. The classification — the
// row's HeavyContent flag — was computed once in snapshotTurns so
// `memory.list` and `memory.get` agree. A heavy row is routed through
// source-bound read surface by reference; a light row is inlined.
//
// Defence in depth (CLAUDE.md §13): when the row is NOT flagged
// heavy yet its materialised value bytes nonetheless meet or exceed the
// threshold, Get fails loudly with ErrContextLeak rather than inline
// the heavy bytes — mirrors the LLM-edge enforcement pass in
// `internal/llm/safety.go`. This catches a future driver / projection
// bug that would let a heavy value reach the inline path.
func buildDetail(deps GetDeps, row projectedTurn, id identity.Quadruple) (prototypes.MemoryItemDetail, error) {
	item := row.item
	detail := prototypes.MemoryItemDetail{
		Item: item,
		Metadata: prototypes.MemoryMetadata{
			StrategyConfig: map[string]string{"strategy": item.Strategy},
		},
	}

	if item.HeavyContent {
		ref, err := memory.SourceReference(id, memory.Item{Key: item.Key, Value: row.value, ExpiresAt: item.ExpiresAt})
		if err != nil {
			return prototypes.MemoryItemDetail{}, err
		}
		detail.ValueArtifact = &prototypes.MemoryArtifactRef{ID: ref.ID, MimeType: ref.MimeType, SizeBytes: ref.SizeBytes, SHA256: ref.SHA256}
		// Inline Value MUST stay empty — exactly one of Value /
		// ValueArtifact is populated.
		detail.Value = nil
		return detail, nil
	}

	// Light row — inline. Defence in depth: the materialised value
	// MUST genuinely be below the threshold. A row that was not
	// classified heavy yet carries heavy bytes is a leak — fail
	// loudly rather than inline it.
	if len(row.value) >= deps.HeavyThreshold {
		return prototypes.MemoryItemDetail{}, fmt.Errorf("%w: key=%s size=%d threshold=%d",
			ErrContextLeak, item.Key, len(row.value), deps.HeavyThreshold)
	}
	detail.Value = row.value
	return detail, nil
}
