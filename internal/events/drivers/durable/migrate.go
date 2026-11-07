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

// errMigrationErased reports that the session was fenced or erased (its
// erasure epoch moved) while it was being migrated.
var errMigrationErased = errors.New("durable: session erased during index migration")

// migrationGuard binds every migration write to the session's erasure state
// captured before the first write: the fence slot must stay absent and the
// erasure epoch unchanged, checked atomically inside each write transaction.
// An erasure (Fence, then DeleteScope) therefore either precedes a write,
// which then fails, or follows it and removes what it wrote. written records
// the chunk generations this migration committed, for a best-effort cleanup
// when the flip finds the session gone.
type migrationGuard struct {
	fence   state.SlotExpectation
	epoch   state.SlotExpectation
	written map[string]state.EventID
}

func (b *bus) captureMigrationGuard(ctx context.Context, id identity.Quadruple) (*migrationGuard, error) {
	fence := state.InternalSlotExpectation(sequenceAuthorityIdentity, durableFenceKind(id.Identity), "")
	if _, err := b.store.Load(ctx, fence.Identity, fence.Kind); err == nil {
		return nil, errMigrationErased
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("load session fence: %w", err)
	}
	_, epoch, err := b.loadErasureEpoch(ctx, id.Identity)
	if err != nil {
		return nil, err
	}
	return &migrationGuard{fence: fence, epoch: epoch, written: map[string]state.EventID{}}, nil
}

// stillValid re-reads the fence and epoch after a conditional write failed.
func (g *migrationGuard) stillValid(ctx context.Context, b *bus, id identity.Quadruple) (bool, error) {
	if _, err := b.store.Load(ctx, g.fence.Identity, g.fence.Kind); err == nil {
		return false, nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return false, fmt.Errorf("load session fence: %w", err)
	}
	_, epoch, err := b.loadErasureEpoch(ctx, id.Identity)
	if err != nil {
		return false, err
	}
	return epoch.ExpectedEventID == g.epoch.ExpectedEventID, nil
}

// removeWritten deletes the chunks this migration committed (exact
// generations only). Used when the session vanished before the flip.
func (b *bus) removeWritten(ctx context.Context, id identity.Quadruple, g *migrationGuard) {
	for kind, eventID := range g.written {
		if _, err := b.store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: kind, ExpectedEventID: eventID}); err != nil && ctx.Err() == nil {
			b.logger.WarnContext(ctx, "durable: could not remove a pre-written index chunk of an erased session",
				slog.String("driver", "durable"), slog.String("head_identity_hash", headIdentityHash(id)), slog.String("error", err.Error()))
		}
	}
}

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
	// Keep only identities and sizes: the scan returned every legacy head's
	// full bytes, which must not stay referenced for the whole pass.
	recs = nil
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
			b.maint.stuck.Store(c.key, struct{}{})
			b.logger.WarnContext(ctx, "durable: index migration failed; session stays in the legacy format and publishes keep working",
				slog.String("driver", "durable"),
				slog.String("head_identity_hash", headIdentityHash(c.id)),
				slog.Duration("retry_after", bo.delay),
				slog.String("error", err.Error()))
			continue
		}
		delete(b.maint.backoff, c.key)
		b.maint.stuck.Delete(c.key)
		if flipped {
			stats.Migrated++
		}
		// Pace after a large session so back-to-back migrations do not
		// monopolize a pool connection and the CPU.
		if c.size > largeMigrationBytes {
			if err := sleepCtx(ctx, b.maint.sessionPause); err != nil {
				return stats, err
			}
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
func (b *bus) migrateSession(ctx context.Context, id identity.Quadruple) (flipped bool, retErr error) {
	guard, err := b.captureMigrationGuard(ctx, id)
	if errors.Is(err, errMigrationErased) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if errors.Is(retErr, errMigrationErased) {
			b.removeWritten(context.WithoutCancel(ctx), id, guard)
			flipped, retErr = false, nil
		}
	}()
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
	// expected is the legacy generation the flip is conditioned on. The
	// first flip attempt uses it without reloading the head: reloading a
	// large legacy head just to read its generation costs a second full
	// copy, and the CAS checks the generation anyway.
	expected := rec.ID
	rows, err := b.loadLegacyRows(ctx, id, rec.Bytes, nil)
	rec.Bytes = nil // release the raw head as soon as it is decoded
	if errors.Is(err, errLegacyNeedsBackfill) {
		// The canonical backfill wrote checkpoints, so the generation moved.
		expected = ""
		rows, err = b.backfilledLegacyRows(ctx, id)
	}
	if err != nil {
		return false, err
	}
	chunks, err := sealRange(rows, nil)
	if err != nil {
		return false, err
	}
	if err := b.prewriteChunks(ctx, id, chunks, guard); err != nil {
		if errors.Is(err, errAlreadyMigrated) {
			return false, nil
		}
		return false, err
	}

	for attempt := 1; attempt <= migrationFlipAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if expected == "" {
			// A concurrent legacy append (or the backfill) moved the head:
			// re-read it and stream only the new rows on top of the
			// already-verified prefix.
			current, err := b.store.Load(ctx, id, kindHead)
			if errors.Is(err, state.ErrNotFound) {
				return false, errMigrationErased // erased meanwhile: drop pre-writes
			}
			if err != nil {
				return false, fmt.Errorf("reload head before flip: %w", err)
			}
			if isIndexV2(current.Bytes) {
				return false, nil
			}
			expected = current.ID
			grown, err := b.loadLegacyRows(ctx, id, current.Bytes, rows)
			current.Bytes = nil
			if errors.Is(err, errLegacyNeedsBackfill) {
				return false, fmt.Errorf("legacy head lost its authenticated metadata before the flip")
			}
			if err != nil {
				return false, err
			}
			rows = grown
			if full := len(rows) / indexChunkRows; full > len(chunks) {
				extra, err := sealRange(rows, chunks)
				if err != nil {
					return false, err
				}
				if err := b.prewriteChunks(ctx, id, extra, guard); err != nil {
					if errors.Is(err, errAlreadyMigrated) {
						return false, nil
					}
					return false, err
				}
				chunks = append(chunks, extra...)
			}
		}
		hdr, err := buildMigratedHeader(rows, chunks)
		if err != nil {
			return false, err
		}
		if err := verifyMigratedHeader(rows, chunks, hdr); err != nil {
			return false, err
		}
		headerBytes, err := encodeHeader(hdr)
		if err != nil {
			return false, err
		}
		next := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headerBytes}
		saveErr := b.store.SaveIf(ctx, []state.SlotExpectation{{Identity: id, Kind: kindHead, ExpectedEventID: expected}, guard.fence, guard.epoch}, next)
		switch {
		case saveErr == nil:
			return true, b.verifyFlippedHeader(ctx, id)
		case errors.Is(saveErr, state.ErrConditionFailed):
			if valid, err := guard.stillValid(ctx, b, id); err != nil {
				return false, err
			} else if !valid {
				return false, errMigrationErased
			}
			expected = "" // concurrent legacy append or another flip: re-read
			continue
		case errors.Is(saveErr, state.ErrCommitOutcomeUnknown):
			landed, err := b.reconcileFlip(ctx, id, next.ID)
			if err != nil {
				return false, fmt.Errorf("flip outcome unknown and unresolved: %w", err)
			}
			if landed {
				return true, b.verifyFlippedHeader(ctx, id)
			}
			expected = ""
			continue
		default:
			return false, fmt.Errorf("flip header: %w", saveErr)
		}
	}
	return false, fmt.Errorf("flip exceeded %d CAS attempts under concurrent appends: %w", migrationFlipAttempts, state.ErrConditionFailed)
}

// errLegacyNeedsBackfill reports a legacy head whose metadata projection is
// not authenticated; it must go through the canonical body backfill first.
var errLegacyNeedsBackfill = errors.New("durable: legacy head needs metadata backfill")

// legacyRowsSpare is capacity reserved for rows appended during migration.
const legacyRowsSpare = 4 * indexChunkRows

// loadLegacyRows authenticates raw legacy head bytes and returns its rows,
// streaming when the shape allows (legacystream.go) and otherwise through the
// ordinary decoder. With prefix, the head must extend prefix unchanged.
func (b *bus) loadLegacyRows(ctx context.Context, id identity.Quadruple, raw []byte, prefix []indexRow) ([]indexRow, error) {
	rows, err := streamLegacyRows(raw, id, prefix, legacyRowsSpare)
	if err == nil || !errors.Is(err, errLegacyStreamUnsupported) {
		return rows, err
	}
	head, err := decodeHead(raw)
	if err != nil {
		return nil, err
	}
	if !headMetadataReady(head) {
		return nil, errLegacyNeedsBackfill
	}
	all, err := legacyRows(head)
	if err != nil {
		return nil, err
	}
	if prefix != nil {
		if len(all) < len(prefix) {
			return nil, integrityErrorf("legacy head shrank below the pre-written prefix")
		}
		for i := range prefix {
			if all[i] != prefix[i] {
				return nil, integrityErrorf("legacy head prefix changed at row %d", i)
			}
		}
	}
	return all, nil
}

// backfilledLegacyRows runs the canonical body backfill (which validates
// every body and checkpoints) and returns the authenticated rows. A missing
// or mismatching body fails loudly and the head is not migrated.
func (b *bus) backfilledLegacyRows(ctx context.Context, id identity.Quadruple) ([]indexRow, error) {
	rec, err := b.store.Load(ctx, id, kindHead)
	if err != nil {
		return nil, fmt.Errorf("reload head for backfill: %w", err)
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		return nil, err
	}
	if head, err = b.ensureHeadMetadata(ctx, id, head); err != nil {
		return nil, fmt.Errorf("authenticate legacy head: %w", err)
	}
	return legacyRows(head)
}

// sealRange seals the full chunks of rows that follow the already sealed
// chunks, continuing their chain. Sealed chunks reference rows without
// copying them.
func sealRange(rows []indexRow, sealed []indexChunk) ([]indexChunk, error) {
	prev := genesisDigest
	if len(sealed) > 0 {
		prev = sealed[len(sealed)-1].Digest
	}
	var out []indexChunk
	for i := len(sealed); i < len(rows)/indexChunkRows; i++ {
		c, err := sealChunk(uint64(i), prev, rows[i*indexChunkRows:(i+1)*indexChunkRows])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		prev = c.Digest
	}
	return out, nil
}

// verifyMigratedHeader checks, before the flip, that the new index re-expands
// to exactly the authenticated legacy rows (chunks then tail, in order) and
// that counts and bounds match. The rows were authenticated against the
// legacy integrity checksum when they were decoded.
func verifyMigratedHeader(rows []indexRow, chunks []indexChunk, hdr indexHeader) error {
	i := 0
	check := func(seg []indexRow) error {
		for _, r := range seg {
			if i >= len(rows) || r != rows[i] {
				return integrityErrorf("migrated index row %d differs from the legacy row", i)
			}
			i++
		}
		return nil
	}
	for _, c := range chunks {
		if err := check(c.Rows); err != nil {
			return err
		}
	}
	if err := check(hdr.Tail); err != nil {
		return err
	}
	if i != len(rows) {
		return integrityErrorf("migrated index re-expands to %d rows, legacy has %d", i, len(rows))
	}
	if hdr.RetainedCount != uint64(len(rows)) {
		return integrityErrorf("migrated retained_count=%d, legacy has %d", hdr.RetainedCount, len(rows))
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
func (b *bus) prewriteChunks(ctx context.Context, id identity.Quadruple, chunks []indexChunk, guard *migrationGuard) error {
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
		err := b.store.SaveBatchIf(ctx, append(expectations, guard.fence, guard.epoch), writes)
		if err == nil {
			for _, w := range writes {
				guard.written[w.Kind] = w.ID
			}
			if err := b.pauseBetweenBatches(ctx); err != nil {
				return err
			}
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !errors.Is(err, state.ErrConditionFailed) && !errors.Is(err, state.ErrCommitOutcomeUnknown) {
			return fmt.Errorf("pre-write chunks [%d,%d): %w", batch[0].Index, batch[len(batch)-1].Index+1, err)
		}
		// Some slot already exists, the session was erased, another process
		// finished the migration, or the outcome is unknown: check the guard
		// and the header, then settle each chunk individually (reuse
		// identical, write absent).
		if valid, err := guard.stillValid(ctx, b, id); err != nil {
			return err
		} else if !valid {
			return errMigrationErased
		}
		if head, err := b.store.Load(ctx, id, kindHead); err == nil && isIndexV2(head.Bytes) {
			return errAlreadyMigrated
		} else if errors.Is(err, state.ErrNotFound) {
			return errMigrationErased
		}
		for i, c := range batch {
			if err := b.settleChunk(ctx, id, c.Index, encoded[i], guard); err != nil {
				return err
			}
		}
	}
	return nil
}

// pauseBetweenBatches paces migration writes so a large migration shares the
// connection pool and CPU with live traffic.
func (b *bus) pauseBetweenBatches(ctx context.Context) error {
	return sleepCtx(ctx, b.maint.prewritePause)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *bus) settleChunk(ctx context.Context, id identity.Quadruple, index uint64, want []byte, guard *migrationGuard) error {
	kind := chunkKind(index)
	for attempt := 1; attempt <= 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := b.store.Load(ctx, id, kind)
		if errors.Is(err, state.ErrNotFound) {
			next := state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kind, Bytes: want, UpdatedAt: b.clock.Now()}
			saveErr := b.store.SaveIf(ctx, []state.SlotExpectation{{Identity: id, Kind: kind}, guard.fence, guard.epoch}, next)
			if saveErr == nil {
				guard.written[kind] = next.ID
				return nil
			}
			if errors.Is(saveErr, state.ErrConditionFailed) || errors.Is(saveErr, state.ErrCommitOutcomeUnknown) {
				if valid, err := guard.stillValid(ctx, b, id); err != nil {
					return err
				} else if !valid {
					return errMigrationErased
				}
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
