package durable

// Background v1 → v2 index migration (D-492, plan §H4.4). Each legacy head is
// converted by (1) authenticating it against bodies when needed, (2) building
// its deterministic sealed chunks, (3) pre-writing those chunks expected-absent
// in bounded batches — unreferenced, so invisible to readers — and (4) one
// small header CAS conditioned on the legacy head's generation. A concurrent
// legacy append only changes the tail, so the flip is retried with the same
// prefix. Any failure leaves the session fully legacy (plus reusable,
// byte-identical orphan chunks) and publishes keep appending in the legacy
// format; the session backs off instead of retrying on the publish path.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// errAlreadyMigrated reports that another writer flipped the session first.
var errAlreadyMigrated = errors.New("durable: session index already migrated")

// migrationStats summarises one migrator pass.
type migrationStats struct {
	Candidates int
	Migrated   int
	Failed     int
	Deferred   int
}

func sessionMapKey(id identity.Quadruple) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
}

// runMigrationPass migrates every legacy head, smallest first, honouring
// per-session backoff. It returns early only on cancellation.
func (b *bus) runMigrationPass(ctx context.Context) (migrationStats, error) {
	var stats migrationStats
	if b.store == nil {
		return stats, nil
	}
	b.maint.passMu.Lock()
	defer b.maint.passMu.Unlock()
	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
	if err != nil {
		return stats, fmt.Errorf("durable: migration: scan head records: %w", err)
	}
	fences, err := b.loadDurableFenceSnapshot(ctx)
	if err != nil {
		return stats, fmt.Errorf("durable: migration: %w", err)
	}
	type candidate struct {
		id   identity.Quadruple
		size int
		key  string
	}
	var cands []candidate
	for _, rec := range recs {
		if rec.Kind != kindHead || isIndexV2(rec.Bytes) || fences.contains(rec.Identity) {
			continue
		}
		cands = append(cands, candidate{id: rec.Identity, size: len(rec.Bytes), key: sessionMapKey(rec.Identity)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].size != cands[j].size {
			return cands[i].size < cands[j].size
		}
		return cands[i].key < cands[j].key
	})
	stats.Candidates = len(cands)
	now := b.clock.Now()
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if bo, ok := b.maint.backoff[c.key]; ok && now.Before(bo.next) {
			stats.Deferred++
			continue
		}
		flipped, err := b.migrateSession(ctx, c.id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stats, ctxErr
			}
			stats.Failed++
			bo := b.maint.backoff[c.key]
			bo.delay = nextMigrationBackoff(bo.delay)
			bo.next = b.clock.Now().Add(bo.delay)
			b.maint.backoff[c.key] = bo
			b.logger.WarnContext(ctx, "durable: index migration failed; session stays in the legacy format and publishes keep working",
				slog.String("driver", "durable"),
				slog.String("head_identity_hash", headIdentityHash(c.id)),
				slog.Duration("retry_after", bo.delay),
				slog.String("error", err.Error()))
			continue
		}
		delete(b.maint.backoff, c.key)
		if flipped {
			stats.Migrated++
		}
	}
	if stats.Candidates > 0 {
		b.logger.InfoContext(ctx, "durable: index migration pass",
			slog.String("driver", "durable"),
			slog.Int("candidates", stats.Candidates),
			slog.Int("migrated", stats.Migrated),
			slog.Int("failed", stats.Failed),
			slog.Int("deferred", stats.Deferred))
	}
	return stats, nil
}

func nextMigrationBackoff(previous time.Duration) time.Duration {
	if previous <= 0 {
		return migrationBackoffBase
	}
	next := previous * 2
	if next > migrationBackoffMax {
		return migrationBackoffMax
	}
	return next
}

// migrateSession converts one legacy head. It reports flipped=false with a
// nil error when there was nothing to do (absent, already v2, or another
// writer migrated it first).
func (b *bus) migrateSession(ctx context.Context, id identity.Quadruple) (bool, error) {
	rec, err := b.store.Load(ctx, id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load head: %w", err)
	}
	if isIndexV2(rec.Bytes) {
		return false, nil
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		return false, err
	}
	if !headMetadataReady(head) {
		// Canonical backfill: validates every body and checkpoints. A missing
		// or mismatching body fails loudly and the head is not migrated.
		if head, err = b.ensureHeadMetadata(ctx, id, head); err != nil {
			return false, fmt.Errorf("authenticate legacy head: %w", err)
		}
	}
	rows, err := legacyRows(head)
	if err != nil {
		return false, err
	}
	chunks, err := sealedChunksFor(rows)
	if err != nil {
		return false, err
	}
	if err := b.prewriteChunks(ctx, id, chunks); err != nil {
		if errors.Is(err, errAlreadyMigrated) {
			return false, nil
		}
		return false, err
	}
	prefix := rows[:len(chunks)*indexChunkRows]

	for attempt := 1; attempt <= migrationFlipAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		current, err := b.store.Load(ctx, id, kindHead)
		if errors.Is(err, state.ErrNotFound) {
			return false, nil // erased meanwhile
		}
		if err != nil {
			return false, fmt.Errorf("reload head before flip: %w", err)
		}
		if isIndexV2(current.Bytes) {
			return false, nil
		}
		legacy, currentRows := head, rows
		if current.ID != rec.ID {
			// A concurrent legacy append landed: decode the new generation.
			// Its integrity checksum is verified once, over the re-expanded
			// index, in verifyMigratedHeader (the same digest
			// headMetadataReady computes), so a large head is not
			// re-serialized twice per attempt.
			if legacy, err = decodeHead(current.Bytes); err != nil {
				return false, err
			}
			if !legacy.MetadataReady || legacy.MetadataValidatedCount != len(legacy.Sequences) || len(legacy.Metadata) != len(legacy.Sequences) {
				return false, fmt.Errorf("legacy head lost its authenticated metadata before the flip")
			}
			if currentRows, err = legacyRows(legacy); err != nil {
				return false, err
			}
		}
		// Legacy heads are append-only: the pre-written prefix must still
		// be the head's prefix.
		if len(currentRows) < len(prefix) {
			return false, integrityErrorf("legacy head shrank below the pre-written prefix")
		}
		for i := range prefix {
			if currentRows[i] != prefix[i] {
				return false, integrityErrorf("legacy head prefix changed at row %d", i)
			}
		}
		if full := len(currentRows) / indexChunkRows; full > len(chunks) {
			all, err := sealedChunksFor(currentRows)
			if err != nil {
				return false, err
			}
			if err := b.prewriteChunks(ctx, id, all[len(chunks):]); err != nil {
				if errors.Is(err, errAlreadyMigrated) {
					return false, nil
				}
				return false, err
			}
			chunks = all
			prefix = currentRows[:len(chunks)*indexChunkRows]
		}
		hdr, err := buildMigratedHeader(currentRows, chunks)
		if err != nil {
			return false, err
		}
		// The initially loaded head was authenticated above
		// (headMetadataReady or the canonical backfill), so its checksum is
		// not recomputed; a newer generation is authenticated here.
		if err := verifyMigratedHeader(id, legacy, currentRows, chunks, hdr, current.ID != rec.ID); err != nil {
			return false, err
		}
		headerBytes, err := encodeHeader(hdr)
		if err != nil {
			return false, err
		}
		next := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headerBytes}
		saveErr := b.store.SaveIf(ctx, []state.SlotExpectation{{Identity: id, Kind: kindHead, ExpectedEventID: current.ID}}, next)
		switch {
		case saveErr == nil:
			return true, b.verifyFlippedHeader(ctx, id)
		case errors.Is(saveErr, state.ErrConditionFailed):
			continue // concurrent legacy append, erasure, or another flip
		case errors.Is(saveErr, state.ErrCommitOutcomeUnknown):
			landed, err := b.reconcileFlip(ctx, id, next.ID)
			if err != nil {
				return false, fmt.Errorf("flip outcome unknown and unresolved: %w", err)
			}
			if landed {
				return true, b.verifyFlippedHeader(ctx, id)
			}
			continue
		default:
			return false, fmt.Errorf("flip header: %w", saveErr)
		}
	}
	return false, fmt.Errorf("flip exceeded %d CAS attempts under concurrent appends: %w", migrationFlipAttempts, state.ErrConditionFailed)
}

// verifyMigratedHeader checks, before the flip, that re-expanding the new
// index reproduces the legacy rows exactly and that the legacy integrity
// checksum still authenticates them, plus counts and bounds.
func verifyMigratedHeader(id identity.Quadruple, legacy headRecord, rows []indexRow, chunks []indexChunk, hdr indexHeader, authenticate bool) error {
	expanded := make([]indexRow, 0, len(rows))
	for _, c := range chunks {
		expanded = append(expanded, c.Rows...)
	}
	expanded = append(expanded, hdr.Tail...)
	if len(expanded) != len(rows) {
		return integrityErrorf("migrated index re-expands to %d rows, legacy has %d", len(expanded), len(rows))
	}
	for i, r := range expanded {
		if r != rows[i] {
			return integrityErrorf("migrated index row %d differs from the legacy row", i)
		}
	}
	if authenticate {
		seqs := make([]uint64, len(expanded))
		metadata := make([]eventMetadataRecord, len(expanded))
		for i, r := range expanded {
			seqs[i] = r.Seq
			metadata[i] = r.metadataRecord(id)
		}
		if metadataIntegrityChecksum(seqs, metadata) != legacy.MetadataIntegrityChecksum {
			return integrityErrorf("migrated index does not reproduce the legacy integrity checksum")
		}
	}
	if hdr.RetainedCount != uint64(len(legacy.Sequences)) {
		return integrityErrorf("migrated retained_count=%d, legacy has %d", hdr.RetainedCount, len(legacy.Sequences))
	}
	if len(rows) > 0 && (hdr.FirstSeq != rows[0].Seq || hdr.LastSeq != rows[len(rows)-1].Seq) {
		return integrityErrorf("migrated bounds disagree with the legacy head")
	}
	return nil
}

// verifyFlippedHeader re-reads the committed header and checks its digest
// and the newest chunk link. A mismatch here is a bug and fails loudly.
func (b *bus) verifyFlippedHeader(ctx context.Context, id identity.Quadruple) error {
	view, err := b.loadIndexView(ctx, id)
	if err != nil {
		return fmt.Errorf("verify migrated header: %w", err)
	}
	if !view.exists || view.legacy {
		return nil // erased or replaced meanwhile
	}
	if view.hdr.NextChunk > view.hdr.FirstChunk {
		it := b.newRowIterator(ctx, &view)
		it.pos = 0 // skip the tail; load and verify the newest chunk
		if _, _, err := it.next(); err != nil {
			var miss *indexMissError
			if errors.As(err, &miss) {
				if classifyErr := b.classifyMissErr(ctx, err); classifyErr == nil {
					return nil
				}
			}
			return b.noteIntegrity(ctx, id, fmt.Errorf("%w: migrated header's newest chunk: %v", ErrIndexIntegrity, err))
		}
	}
	return nil
}

func (b *bus) reconcileFlip(ctx context.Context, id identity.Quadruple, flipped state.EventID) (bool, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitReconcileTimeout)
	defer cancel()
	rec, err := b.store.Load(rctx, id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return rec.ID == flipped, nil
}

// prewriteChunks writes chunks expected-absent in bounded batches. A slot that
// already holds byte-identical bytes (an earlier attempt, or another
// migrator) is reused. A differing slot means the session was migrated by
// someone else (header already v2) or holds a stale orphan.
func (b *bus) prewriteChunks(ctx context.Context, id identity.Quadruple, chunks []indexChunk) error {
	for start := 0; start < len(chunks); start += migrationPrewriteBatch {
		end := min(start+migrationPrewriteBatch, len(chunks))
		batch := chunks[start:end]
		encoded := make([][]byte, len(batch))
		expectations := make([]state.SlotExpectation, len(batch))
		writes := make([]state.StateRecord, len(batch))
		for i, c := range batch {
			raw, err := encodeChunk(c)
			if err != nil {
				return err
			}
			encoded[i] = raw
			expectations[i] = state.SlotExpectation{Identity: id, Kind: chunkKind(c.Index)}
			writes[i] = state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: chunkKind(c.Index), Bytes: raw, UpdatedAt: b.clock.Now()}
		}
		err := b.store.SaveBatchIf(ctx, expectations, writes)
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !errors.Is(err, state.ErrConditionFailed) && !errors.Is(err, state.ErrCommitOutcomeUnknown) {
			return fmt.Errorf("pre-write chunks [%d,%d): %w", batch[0].Index, batch[len(batch)-1].Index+1, err)
		}
		// Some slot already exists, or the outcome is unknown: settle each
		// chunk individually (reuse identical, write absent).
		for i, c := range batch {
			if err := b.settleChunk(ctx, id, c.Index, encoded[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *bus) settleChunk(ctx context.Context, id identity.Quadruple, index uint64, want []byte) error {
	kind := chunkKind(index)
	for attempt := 1; attempt <= 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := b.store.Load(ctx, id, kind)
		if errors.Is(err, state.ErrNotFound) {
			saveErr := b.store.SaveIf(ctx, []state.SlotExpectation{{Identity: id, Kind: kind}},
				state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kind, Bytes: want, UpdatedAt: b.clock.Now()})
			if saveErr == nil {
				return nil
			}
			if errors.Is(saveErr, state.ErrConditionFailed) || errors.Is(saveErr, state.ErrCommitOutcomeUnknown) {
				continue // re-read decides
			}
			return fmt.Errorf("pre-write chunk %d: %w", index, saveErr)
		}
		if err != nil {
			return fmt.Errorf("load chunk %d: %w", index, err)
		}
		if bytes.Equal(rec.Bytes, want) {
			return nil
		}
		head, err := b.store.Load(ctx, id, kindHead)
		if err == nil && isIndexV2(head.Bytes) {
			return errAlreadyMigrated
		}
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("reload head for chunk %d: %w", index, err)
		}
		// The session is still legacy, so no header references this slot. A
		// stale orphan (from a head an offline repair later changed) is only
		// replaced once it is old enough that no concurrent migration of
		// the current prefix could have produced it.
		if !rec.UpdatedAt.IsZero() && b.clock.Now().Sub(rec.UpdatedAt) >= orphanChunkMinAge {
			if _, err := b.store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: kind, ExpectedEventID: rec.ID}); err != nil {
				return fmt.Errorf("remove stale orphan chunk %d: %w", index, err)
			}
			continue
		}
		return integrityErrorf("pre-existing chunk %d differs from the legacy prefix", index)
	}
	return fmt.Errorf("pre-write chunk %d did not settle: %w", index, state.ErrConditionFailed)
}
