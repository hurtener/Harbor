package protocol

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// HealthDeps carries the dependencies Health composes over.
type HealthDeps struct {
	// Store is the memory subsystem the snapshot + health are read
	// from.
	Store memory.MemoryStore
	// Aggregator is the events Aggregator the 24-hour counters derive
	// from. Optional — see ListDeps.Aggregator.
	Aggregator *events.Aggregator
	// DriverByScope is the configured per-scope driver mapping —
	// e.g. {"session":"inmem", "tenant":"postgres"}. The caller
	// supplies it from config; the MemoryStore interface does not
	// expose a per-scope driver split. When unset, Health reports a
	// single-scope mapping for the session scope under DriverName.
	DriverByScope map[string]string
	// DriverName is the configured memory-driver name, used to seed a
	// single-scope DriverByScope mapping when DriverByScope is unset.
	DriverName string
}

// Health answers the `memory.health` Protocol method: it returns the
// aggregate memory-health counters (total records / expiring-in-1h /
// identity-rejected-24h / recovery-dropped-24h) plus the per-scope
// driver mapping.
//
// Identity is mandatory (D-001): an incomplete triple on id fails
// loudly with `memory.ErrIdentityRequired`. The record counters derive
// from the caller's per-identity snapshot; the 24-hour event counters
// derive from the events Aggregator (when wired); the driver mapping
// derives from the configured per-scope driver split.
func Health(ctx context.Context, deps HealthDeps, id identity.Quadruple) (prototypes.MemoryHealthResponse, error) {
	if deps.Store == nil {
		return prototypes.MemoryHealthResponse{}, fmt.Errorf("memory/protocol: Health: Store is nil")
	}
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryHealthResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return prototypes.MemoryHealthResponse{}, err
	}

	// Touch Health so a driver-side failure (closed store, etc.)
	// surfaces loudly rather than being masked by an all-zero counter
	// roll-up.
	if _, err := deps.Store.Health(ctx, id); err != nil {
		return prototypes.MemoryHealthResponse{}, fmt.Errorf("memory/protocol: Health: store health: %w", err)
	}

	snap, err := deps.Store.Snapshot(ctx, id)
	if err != nil {
		return prototypes.MemoryHealthResponse{}, fmt.Errorf("memory/protocol: Health: snapshot: %w", err)
	}
	// Health's record-count roll-up does not depend on the heavy-
	// content flag; pass 0 (no per-row heavy classification needed).
	rows, err := snapshotTurns(snap, id, deps.DriverName, 0)
	if err != nil {
		return prototypes.MemoryHealthResponse{}, err
	}

	now := time.Now().UTC()
	expiring := int64(0)
	for _, r := range rows {
		if ttlExpiringWithin(r.item.ExpiresAt, now, ttlExpiryWindow) {
			expiring++
		}
	}
	rejected, dropped := eventCounters(ctx, deps.Aggregator, id)

	return prototypes.MemoryHealthResponse{
		Aggregate: prototypes.MemoryHealthAggregate{
			Total:               int64(len(rows)),
			ExpiringIn1h:        expiring,
			IdentityRejected24h: rejected,
			RecoveryDropped24h:  dropped,
			DriverByScope:       driverByScope(deps),
		},
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// driverByScope returns the per-scope driver mapping. When the caller
// supplied an explicit DriverByScope it is used verbatim; otherwise a
// single-scope mapping for the session scope under DriverName is
// returned (memory is session-scoped by default — CLAUDE.md §6 rule 4).
// The returned map is a defensive copy so a caller cannot mutate the
// shared dependency.
func driverByScope(deps HealthDeps) map[string]string {
	if len(deps.DriverByScope) > 0 {
		out := make(map[string]string, len(deps.DriverByScope))
		for k, v := range deps.DriverByScope {
			out[k] = v
		}
		return out
	}
	name := deps.DriverName
	if name == "" {
		name = string(prototypes.MemoryDriverInmem)
	}
	return map[string]string{string(prototypes.MemoryScopeSession): name}
}
