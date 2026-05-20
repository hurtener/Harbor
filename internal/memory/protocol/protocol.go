// Package protocol composes the read-only Console-memory-page Protocol
// surface (Phase 73j / D-118) on top of the shipped memory subsystem.
//
// It exposes three pure functions — List, Get, Health — that the
// Protocol stream-transport handlers (`internal/protocol/transports/
// stream/memory_handler.go`) call to answer the `memory.list` /
// `memory.get` / `memory.health` methods. The functions are stateless:
// every dependency is passed in per call, nothing is cached on a
// package-level value, and the compiled artifacts they consume
// (MemoryStore, ArtifactStore, the events Aggregator) are themselves
// D-025-safe.
//
// # The projection model
//
// The shipped `memory.MemoryStore` interface (Phases 23–25) is
// per-identity: it has no per-item enumeration method. It exposes
// `Snapshot(ctx, id)` — an opaque JSON `memory.Record{Strategy, Turns}`
// — and `Health(ctx, id)`. This package projects that record into the
// Console-page row shape: each conversation turn in a snapshot becomes
// one `MemoryItem` row, keyed by a deterministic per-turn key
// (`memTurnKey`). The rolling-summary text, when present, is NOT a
// separate row — it is folded into the strategy metadata. This is the
// honest projection: the runtime's memory state is conversation turns,
// and the Memory page renders them per-identity.
//
// # Identity is mandatory (D-001 / D-033)
//
// Every function validates the identity quadruple before touching the
// store. A missing tenant / user / session fails loudly with
// `memory.ErrIdentityRequired`; the caller (the stream handler) maps
// that onto the canonical `CodeIdentityRequired` Protocol error. The
// driver layer ALSO emits a `memory.identity_rejected` event on the
// bus (D-033) — this package does not re-emit; it relies on the shipped
// driver-layer emit and never masks the rejection.
//
// # Heavy values bypass via artifacts (D-026)
//
// `Get` mirrors the LLM-edge enforcement pass (`internal/llm/safety.go`):
// a record value whose byte length meets or exceeds the configured
// heavy-content threshold is routed through the ArtifactStore and the
// detail ships a `MemoryArtifactRef` instead of inline bytes. A driver
// that hands back raw heavy bytes that this package would otherwise
// inline is a leak — `Get` fails loudly with `ErrContextLeak` rather
// than inlining (the same posture the LLM edge takes).
//
// # No mutation surface
//
// V1 is read-only. `memory.put` / `memory.delete` are deferred to
// Phase 73 / post-V1 (page-memory.md §10); this package ships no
// mutation path.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrContextLeak — Get materialised a memory record value whose byte
// length meets or exceeds the heavy-content threshold but the value
// reached the response path as raw inline bytes rather than an
// ArtifactStub. Mirrors `llm.ErrContextLeak` (D-026 / CLAUDE.md §13):
// a heavy value MUST route through the ArtifactStore by reference; an
// inline heavy value is a leak and is failed loudly, never truncated.
var ErrContextLeak = errors.New("memory/protocol: heavy memory value reached the response path as raw inline bytes (D-026)")

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

// memTurnKey computes the deterministic per-turn memory key for a
// conversation turn within an identity scope. The key is stable across
// repeated Snapshot calls for the same turn (the input bytes are the
// identity triple + the turn ordinal + the turn timestamp), so a
// `memory.get` round-trip after a `memory.list` resolves the same row.
//
// The form is `mem_{sha256_hex[:16]}` — content-addressed, opaque to
// the Console (a client never constructs or parses it).
func memTurnKey(id identity.Quadruple, ordinal int, ts time.Time) string {
	h := sha256.New()
	// time.Time → a stable byte form; nanos are part of the digest so
	// two turns at the same wall-second still differ.
	seed := fmt.Sprintf("%s|%s|%s|%s|%d|%d",
		id.TenantID, id.UserID, id.SessionID, id.RunID, ordinal, ts.UnixNano())
	h.Write([]byte(seed))
	return "mem_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// turnValueBytes renders a conversation turn into its post-redaction
// value bytes — the canonical JSON shape the Memory page's value viewer
// renders and `memory.get` returns (or routes through artifacts above
// the heavy threshold).
//
// The turn's user / assistant text is included verbatim; the trajectory
// digest, when present, is folded in. This is the value the size /
// heavy-content classification keys on.
func turnValueBytes(turn memory.ConversationTurn) ([]byte, error) {
	view := map[string]any{
		"user_message":       turn.UserMessage,
		"assistant_response": turn.AssistantResponse,
		"timestamp":          turn.Timestamp,
	}
	if turn.TrajectoryDigest != nil {
		view["trajectory_digest"] = map[string]any{
			"tools_invoked":        turn.TrajectoryDigest.ToolsInvoked,
			"observations_summary": turn.TrajectoryDigest.ObservationsSummary,
			"reasoning_summary":    turn.TrajectoryDigest.ReasoningSummary,
			"artifacts_refs":       turn.TrajectoryDigest.ArtifactsRefs,
		}
	}
	if len(turn.ArtifactsHiddenRefs) > 0 {
		view["artifacts_hidden_refs"] = turn.ArtifactsHiddenRefs
	}
	b, err := json.Marshal(view)
	if err != nil {
		return nil, fmt.Errorf("memory/protocol: marshal turn value: %w", err)
	}
	return b, nil
}

// projectedTurn is the internal per-turn projection — a MemoryItem row
// plus the materialised value bytes Get needs. List builds these,
// filters / sorts / paginates them; Get builds one for a target key.
type projectedTurn struct {
	item  prototypes.MemoryItem
	value []byte
}

// snapshotTurns reads the per-identity snapshot from store and projects
// every conversation turn into a projectedTurn. driver is the
// configured driver name (surfaced on each row's Driver field — the
// MemoryStore interface does not expose it, so the caller supplies it).
// heavyThreshold is the D-026 heavy-content byte size; a turn whose
// value bytes meet or exceed it has its row's HeavyContent flag set —
// the SINGLE classification point, so `memory.list` and `memory.get`
// agree on which rows are heavy.
//
// Identity is validated by the caller before this runs; snapshotTurns
// trusts a validated id.
func snapshotTurns(snap memory.Snapshot, id identity.Quadruple, driver string, heavyThreshold int) ([]projectedTurn, error) {
	// An empty snapshot — or one carrying a strategy but no record
	// bytes (the `none` strategy's "never written" return shape, and
	// the `truncation` / `rolling_summary` Restore-of-empty case) —
	// projects to zero rows. Only a snapshot with actual record bytes
	// is decoded.
	if snap.IsEmpty() || len(snap.Bytes) == 0 {
		return nil, nil
	}
	var rec memory.Record
	if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
		return nil, fmt.Errorf("memory/protocol: decode snapshot record: %w", err)
	}
	strat := string(rec.Strategy)
	if strat == "" {
		strat = string(snap.Strategy)
	}
	out := make([]projectedTurn, 0, len(rec.Turns))
	for i, turn := range rec.Turns {
		val, err := turnValueBytes(turn)
		if err != nil {
			return nil, err
		}
		ts := turn.Timestamp
		heavy := heavyThreshold > 0 && len(val) >= heavyThreshold
		out = append(out, projectedTurn{
			item: prototypes.MemoryItem{
				Key:      memTurnKey(id, i, ts),
				Strategy: strat,
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
// Used for the runtime-side ContentSearch facet (brief 11 §CC-4).
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
