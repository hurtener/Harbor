package durable

// Offline single-session index recovery (D-492 amendment). A session whose v2
// header or chunk fails integrity checks is quarantined at boot and refused on
// its own reads and publishes. RebuildSessionIndex recovers just that session
// from its immutable entry bodies — the canonical source — instead of a
// whole-store snapshot restore. Like the legacy repair it runs only with every
// event writer stopped (an explicit acknowledgement), operates purely through
// state.StateStore, and never rewrites or reads out an entry body's payload.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// ErrRebuildRefused means the rebuild contract was not met; nothing was written.
var ErrRebuildRefused = errors.New("durable: session index rebuild refused")

// RebuildSessionIndexOptions controls one offline rebuild.
type RebuildSessionIndexOptions struct {
	// Apply writes the rebuilt index; false only inspects.
	Apply bool
	// WriterDrained must be true for Apply: every event writer for the
	// store is stopped.
	WriterDrained bool
}

// RebuildSessionIndexReport is the content-free outcome of a rebuild.
type RebuildSessionIndexReport struct {
	HeadIdentityHash string `json:"head_identity_hash_sha256"`
	// CurrentIndex is "absent", "legacy", "v2_ok" or "v2_integrity_failure".
	CurrentIndex   string `json:"current_index"`
	Entries        int    `json:"entries"`
	Retained       int    `json:"retained"`
	Chunks         uint64 `json:"chunks"`
	TrimmedCount   uint64 `json:"trimmed_count"`
	TrimmedThrough uint64 `json:"trimmed_through_seq"`
	// TrimmedBodies counts bodies at or below the trimmed cut that the
	// rebuild excludes (and, on apply, deletes).
	TrimmedBodies int  `json:"trimmed_bodies"`
	StaleChunks   int  `json:"stale_chunks_removed"`
	Applied       bool `json:"applied"`
}

// RebuildSessionIndex rebuilds one session's v2 index from its entry bodies.
// The trimmed range is taken from the session's retention summary, so a
// rebuilt session keeps reporting its audit-retention cut.
func RebuildSessionIndex(ctx context.Context, store state.StateStore, id identity.Quadruple, opts RebuildSessionIndexOptions) (RebuildSessionIndexReport, error) {
	report := RebuildSessionIndexReport{HeadIdentityHash: headIdentityHash(id)}
	if store == nil {
		return report, fmt.Errorf("%w: state store is required", ErrRebuildRefused)
	}
	id = sessionKey(id)
	if err := state.ValidateIdentity(id); err != nil {
		return report, fmt.Errorf("%w: complete tenant/user/session identity required", ErrRebuildRefused)
	}
	if opts.Apply && !opts.WriterDrained {
		return report, fmt.Errorf("%w: apply requires explicit writer-drained acknowledgement", ErrRebuildRefused)
	}
	headExpectation := state.SlotExpectation{Identity: id, Kind: kindHead}
	head, err := store.Load(ctx, id, kindHead)
	switch {
	case errors.Is(err, state.ErrNotFound):
		report.CurrentIndex = "absent"
	case err != nil:
		return report, fmt.Errorf("load head: %w", err)
	default:
		headExpectation.ExpectedEventID = head.ID
		report.CurrentIndex = "legacy"
		if isIndexV2(head.Bytes) {
			report.CurrentIndex = "v2_ok"
			if _, decodeErr := decodeHeader(head.Bytes); decodeErr != nil {
				report.CurrentIndex = "v2_integrity_failure"
			}
		}
	}

	b := &bus{store: store, clock: realClock{}}
	var through, trimmedCount uint64
	if summary, _, err := b.loadRetentionSummary(ctx, id); err != nil {
		return report, fmt.Errorf("%w: retention summary: %v", ErrRebuildRefused, err)
	} else if summary.TrimmedCount > 0 {
		through, trimmedCount = summary.LastTrimmedSeq, summary.TrimmedCount
	}

	entries, err := store.ListKindForIdentity(ctx, id, kindEntryPrefix)
	if err != nil {
		return report, fmt.Errorf("list entries: %w", err)
	}
	report.Entries = len(entries)
	var rows []indexRow
	var trimmedBodies []state.StateRecord
	for _, rec := range entries {
		ev, err := decodeEvent(rec.Bytes)
		if err != nil {
			return report, fmt.Errorf("%w: entry %s is malformed", ErrRebuildRefused, rec.Kind)
		}
		if rec.Kind != kindEntryPrefix+seqToken(ev.Sequence) || ev.Identity.Identity != id.Identity || !events.IsValidEventType(ev.Type) {
			return report, fmt.Errorf("%w: entry %s disagrees with its slot or identity", ErrRebuildRefused, rec.Kind)
		}
		if trimmedCount > 0 && ev.Sequence <= through {
			trimmedBodies = append(trimmedBodies, rec)
			continue
		}
		row, err := rowFromEvent(ev)
		if err != nil {
			return report, fmt.Errorf("%w: entry %s: %v", ErrRebuildRefused, rec.Kind, err)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	hdr := newIndexHeader()
	if trimmedCount > 0 {
		hdr.Trimmed = indexTrimmed{Count: trimmedCount, ThroughSeq: through}
		hdr.LastSeq = through
	}
	sealed, err := hdr.appendRows(rows)
	if err != nil {
		return report, fmt.Errorf("%w: %v", ErrRebuildRefused, err)
	}
	headerBytes, err := encodeHeader(hdr)
	if err != nil {
		return report, err
	}
	report.Retained, report.Chunks = len(rows), hdr.NextChunk
	report.TrimmedCount, report.TrimmedThrough, report.TrimmedBodies = trimmedCount, through, len(trimmedBodies)
	if !opts.Apply {
		return report, nil
	}

	// Writers are drained: chunk slots are overwritten in place (their
	// current generation as the expectation), then the header flips with a
	// CAS on the generation inspected above.
	for start := 0; start < len(sealed); start += migrationPrewriteBatch {
		batch := sealed[start:min(start+migrationPrewriteBatch, len(sealed))]
		expectations := make([]state.SlotExpectation, len(batch))
		writes := make([]state.StateRecord, len(batch))
		for i, c := range batch {
			raw, err := encodeChunk(c)
			if err != nil {
				return report, err
			}
			kind := chunkKind(c.Index)
			expectations[i] = state.SlotExpectation{Identity: id, Kind: kind}
			if existing, err := store.Load(ctx, id, kind); err == nil {
				expectations[i].ExpectedEventID = existing.ID
			} else if !errors.Is(err, state.ErrNotFound) {
				return report, fmt.Errorf("load chunk slot %d: %w", c.Index, err)
			}
			writes[i] = state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kind, Bytes: raw}
		}
		if err := store.SaveBatchIf(ctx, expectations, writes); err != nil {
			return report, fmt.Errorf("write rebuilt chunks: %w", err)
		}
	}
	if err := store.SaveIf(ctx, []state.SlotExpectation{headExpectation},
		state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headerBytes}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return report, fmt.Errorf("%w: the head changed during the rebuild; are all writers stopped?", ErrRebuildRefused)
		}
		return report, fmt.Errorf("write rebuilt header: %w", err)
	}
	// Remove chunk slots the new index does not reference and trimmed
	// bodies the sweeper had not deleted yet.
	chunkRecs, err := store.ListKindForIdentity(ctx, id, kindChunkPrefix)
	if err != nil {
		return report, fmt.Errorf("list chunk slots: %w", err)
	}
	for _, rec := range chunkRecs {
		if index, ok := parseChunkKind(rec.Kind); ok && index >= hdr.NextChunk {
			if _, err := store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: rec.Kind, ExpectedEventID: rec.ID}); err != nil {
				return report, fmt.Errorf("remove stale chunk %d: %w", index, err)
			}
			report.StaleChunks++
		}
	}
	for _, rec := range trimmedBodies {
		if _, err := store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: rec.Kind, ExpectedEventID: rec.ID}); err != nil {
			return report, fmt.Errorf("remove trimmed body: %w", err)
		}
	}
	report.Applied = true
	return report, nil
}
