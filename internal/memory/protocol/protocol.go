// Package protocol projects the memory owner's authoritative inspection through
// the existing admin-scoped memory routes. Cumulative checkpoints, recent
// execution and retained evidence are one bounded read, never a second stored
// transcript. The owner supplies stable keys and performs conditional mutations.
// Heavy values remain subject to the Protocol's existing artifact boundary.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrContextLeak — Get materialised a memory record value whose byte
// length meets or exceeds the heavy-content threshold but the value
// reached the response path as raw inline bytes rather than an
// ArtifactStub. Mirrors `llm.ErrContextLeak` (CLAUDE.md §13):
// a heavy value MUST route through the ArtifactStore by reference; an
// inline heavy value is a leak and is failed loudly, never truncated.
var ErrContextLeak = errors.New("memory/protocol: heavy memory value reached the response path as raw inline bytes")

// ErrInvalidFilter — a memory.list filter carried a structurally
// invalid value: an unknown scope / driver / strategy enum, or a
// negative page / page-size. The caller maps this onto
// `CodeInvalidRequest`. Fails loudly — a malformed filter is never a
// silently-dropped facet (CLAUDE.md §13).
var ErrInvalidFilter = errors.New("memory/protocol: invalid memory.list filter")

// ErrPageOutOfRange — a memory.list request asked for a page-size above
// the documented maximum (or a negative page / page-size). Distinct
// from ErrInvalidFilter so the caller can render a precise message;
// both map onto `CodeInvalidRequest`.
var ErrPageOutOfRange = errors.New("memory/protocol: page/page_size out of range")

// projectedTurn is the internal per-turn projection — a MemoryItem row
// plus the materialised value bytes Get needs. List builds these,
// filters / sorts / paginates them; Get builds one for a target key.
type projectedTurn struct {
	item  prototypes.MemoryItem
	value []byte
}

// snapshotTurns supplies the wire-only identity/driver projection and heavy-value
// classification. State selection, keys and retention belong to the memory owner.
func snapshotTurns(snap memory.Inspection, id identity.Quadruple, driver string, heavyThreshold int) ([]projectedTurn, error) {
	out := make([]projectedTurn, 0, len(snap.Items))
	for _, entry := range snap.Items {
		if entry.Key == "" || !json.Valid(entry.Value) {
			return nil, fmt.Errorf("%w: invalid memory inspection item", memory.ErrInvalidSnapshot)
		}
		val, ts := entry.Value, entry.CreatedAt
		heavy := heavyThreshold > 0 && len(val) >= heavyThreshold
		out = append(out, projectedTurn{
			item: prototypes.MemoryItem{
				Key:      entry.Key,
				Strategy: string(snap.Strategy),
				// Memory is session-scoped by default (CLAUDE.md §6
				// rule 4); the snapshot surface is the session record.
				Scope: string(prototypes.MemoryScopeSession),
				Identity: prototypes.IdentityScope{
					Tenant:  id.TenantID,
					User:    id.UserID,
					Session: id.SessionID,
				},
				CreatedAt:     ts,
				LastUpdatedAt: ts,
				ExpiresAt:     entry.ExpiresAt,
				SizeBytes:     int64(len(val)),
				HeavyContent:  heavy,
				Driver:        driver,
			},
			value: val,
		})
	}
	return out, nil
}

// containsFold reports whether haystack contains needle, case-folded.
// Used for the runtime-side ContentSearch facet.
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// sortByLastUpdatedDesc orders rows newest-first for a deterministic
// table. Ties (same LastUpdatedAt) break on Key for stability.
func sortByLastUpdatedDesc(rows []projectedTurn) {
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := rows[i].item.LastUpdatedAt, rows[j].item.LastUpdatedAt
		if ti.Equal(tj) {
			return rows[i].item.Key < rows[j].item.Key
		}
		return ti.After(tj)
	})
}
