package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// memoryValueArtifactNamespace is the artifact namespace heavy memory
// values are routed under (D-026). A dedicated namespace keeps the
// content-addressed IDs distinguishable from other artifact producers.
const memoryValueArtifactNamespace = "memory_value"

// GetDeps carries the dependencies Get composes over.
type GetDeps struct {
	// Store is the memory subsystem the snapshot is projected from.
	Store memory.MemoryStore
	// Artifacts is the ArtifactStore heavy values (≥ HeavyThreshold)
	// are routed through (D-026). Mandatory — a nil fails loud.
	Artifacts artifacts.ArtifactStore
	// DriverName is the configured memory-driver name surfaced on the
	// returned row.
	DriverName string
	// HeavyThreshold is the configured heavy-content byte size
	// (cfg.Artifacts.HeavyOutputThresholdBytes). A value whose byte
	// length meets or exceeds it routes through the ArtifactStore. A
	// non-positive threshold fails loud (a zero threshold would route
	// every value).
	HeavyThreshold int
}

// Get answers the `memory.get` Protocol method: it resolves a single
// memory record by key within the caller's identity scope and returns
// the full detail — metadata + post-redaction value (below the
// heavy-content threshold) OR a `MemoryArtifactRef` (at or above it).
//
// Identity is mandatory (D-001). The heavy-value bypass (D-026) is
// enforced: a record value at or above HeavyThreshold is routed
// through the ArtifactStore and the detail ships `ValueArtifact`; the
// inline `Value` is left empty. EXACTLY ONE of Value / ValueArtifact is
// populated. A value that somehow reached the inline path while being
// heavy is a leak — Get fails loudly with `ErrContextLeak` rather than
// inlining it (mirrors the LLM-edge enforcement in
// `internal/llm/safety.go`).
//
// A key that resolves to no record returns `memory.ErrNotFound` — the
// caller maps it onto `CodeNotFound`.
func Get(ctx context.Context, deps GetDeps, req prototypes.MemoryGetRequest, id identity.Quadruple) (prototypes.MemoryGetResponse, error) {
	if deps.Store == nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: Store is nil")
	}
	if deps.Artifacts == nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: Artifacts is nil")
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

	snap, err := deps.Store.Snapshot(ctx, id)
	if err != nil {
		return prototypes.MemoryGetResponse{}, fmt.Errorf("memory/protocol: Get: snapshot: %w", err)
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

	detail, err := buildDetail(ctx, deps, *target, id)
	if err != nil {
		return prototypes.MemoryGetResponse{}, err
	}
	return prototypes.MemoryGetResponse{
		Detail:          detail,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// buildDetail assembles the MemoryItemDetail for a resolved row,
// applying the D-026 heavy-content bypass. The classification — the
// row's HeavyContent flag — was computed once in snapshotTurns so
// `memory.list` and `memory.get` agree. A heavy row is routed through
// the ArtifactStore by reference; a light row is inlined.
//
// Defence in depth (D-026 / CLAUDE.md §13): when the row is NOT flagged
// heavy yet its materialised value bytes nonetheless meet or exceed the
// threshold, Get fails loudly with ErrContextLeak rather than inline
// the heavy bytes — mirrors the LLM-edge enforcement pass in
// `internal/llm/safety.go`. This catches a future driver / projection
// bug that would let a heavy value reach the inline path.
func buildDetail(ctx context.Context, deps GetDeps, row projectedTurn, id identity.Quadruple) (prototypes.MemoryItemDetail, error) {
	item := row.item
	detail := prototypes.MemoryItemDetail{
		Item: item,
		Metadata: prototypes.MemoryMetadata{
			StrategyConfig: map[string]string{"strategy": item.Strategy},
		},
	}

	if item.HeavyContent {
		// Heavy value — route through the ArtifactStore by reference.
		ref, err := routeHeavyValue(ctx, deps.Artifacts, row.value, id, item.Key)
		if err != nil {
			return prototypes.MemoryItemDetail{}, err
		}
		detail.ValueArtifact = ref
		// Inline Value MUST stay empty — exactly one of Value /
		// ValueArtifact is populated (D-026).
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

// routeHeavyValue stores a heavy memory value in the ArtifactStore and
// returns the by-reference stub. A marshal / store failure fails loud —
// never a silent truncation (D-026, §13).
func routeHeavyValue(ctx context.Context, store artifacts.ArtifactStore, value []byte, id identity.Quadruple, key string) (*prototypes.MemoryArtifactRef, error) {
	scope := artifacts.ArtifactScope{
		TenantID:  id.TenantID,
		UserID:    id.UserID,
		SessionID: id.SessionID,
	}
	ref, err := store.PutBytes(ctx, scope, value, artifacts.PutOpts{
		MimeType:  "application/json",
		Namespace: memoryValueArtifactNamespace,
		Source: map[string]any{
			// The artifact provenance carries the producer + the memory
			// key so an operator can trace the stub back to its record.
			"producer":   "memory.get",
			"memory_key": key,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("memory/protocol: route heavy memory value to artifact store: %w", err)
	}
	return &prototypes.MemoryArtifactRef{
		ID:        ref.ID,
		MimeType:  ref.MimeType,
		SizeBytes: ref.SizeBytes,
		Filename:  ref.Filename,
		SHA256:    ref.SHA256,
	}, nil
}
