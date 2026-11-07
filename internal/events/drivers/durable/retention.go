package durable

// Opt-in audit-trail retention (D-493, plan §H5). A pass traverses every head
// (the same complete maintenance scan boot recovery performs) in a
// deterministic order from an in-memory resume point, so a pass that exhausts
// its delete budget continues where it stopped and no session is starved.
//
// Per v2 session the sweeper verifies the chunk chain from base_digest
// through the cut, then commits the new header and the cumulative summary in
// one SaveBatchIf — before any delete. Deletes follow, idempotent (Load plus
// exact-generation DeleteIf), paced, and budgeted per pass. A crash between
// commit and delete leaves records below first_chunk (or listed in
// pending_entries) that the next pass removes; readers classify any record
// that vanishes mid-read against the reloaded header (indexview.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// errDeleteBudgetExhausted ends a pass's delete phase.
var errDeleteBudgetExhausted = errors.New("durable: retention delete budget exhausted for this pass")

// errSessionSliceExhausted ends one session's share of a pass's deletes.
var errSessionSliceExhausted = errors.New("durable: retention per-session delete slice exhausted")

// retentionSummary is the cumulative per-session record of what audit
// retention removed. It is written in the same transaction as the header
// that performs the trim, so the two never disagree.
type retentionSummary struct {
	Format                 int               `json:"format"`
	TrimmedCount           uint64            `json:"trimmed_count"`
	ByType                 map[string]uint64 `json:"by_type"`
	FirstTrimmedSeq        uint64            `json:"first_trimmed_seq"`
	LastTrimmedSeq         uint64            `json:"last_trimmed_seq"`
	FirstTrimmedOccurredAt int64             `json:"first_trimmed_occurred_at"`
	LastTrimmedOccurredAt  int64             `json:"last_trimmed_occurred_at"`
	// ChainAnchor is the header's base_digest after the latest trim: the
	// digest of the last trimmed chunk (or the unchanged base for a tail trim).
	ChainAnchor string                 `json:"chain_anchor"`
	Policy      retentionSummaryPolicy `json:"policy"`
	LastPassAt  int64                  `json:"last_pass_at"`
	Passes      uint64                 `json:"passes"`
}

type retentionSummaryPolicy struct {
	AuditMaxAge string `json:"audit_max_age"`
}

// retentionStats summarises one sweeper pass (logged as one Info line).
type retentionStats struct {
	SessionsScanned int
	SessionsTrimmed int
	ChunksTrimmed   int
	RowsTrimmed     int
	EntriesDeleted  int
	ChunksDeleted   int
	DeletesDeferred bool
	OrphansCleaned  int
	Complete        bool
}

// deleteBudget bounds and paces storage deletes within one pass.
type deleteBudget struct {
	remaining int
	// sliceLeft bounds one session's deletes so a large backlog cannot use
	// a whole pass's budget while other sessions wait.
	sliceLeft int
	interval  time.Duration
	last      time.Time
}

func (d *deleteBudget) beginSession(slice int) { d.sliceLeft = slice }

func (d *deleteBudget) take(ctx context.Context) error {
	if d.remaining <= 0 {
		return errDeleteBudgetExhausted
	}
	if d.sliceLeft <= 0 {
		return errSessionSliceExhausted
	}
	if d.interval > 0 && !d.last.IsZero() {
		if wait := d.interval - time.Since(d.last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	d.last = time.Now()
	d.remaining--
	d.sliceLeft--
	return nil
}

func rowQualifies(r indexRow, cutoff int64) bool {
	return isAuditType(r.Type) && r.OccurredAt < cutoff
}

func allQualify(rows []indexRow, cutoff int64) bool {
	for _, r := range rows {
		if !rowQualifies(r, cutoff) {
			return false
		}
	}
	return len(rows) > 0
}

// runRetentionPass runs one sweeper pass. It is a no-op when retention is
// disabled.
func (b *bus) runRetentionPass(ctx context.Context) (retentionStats, error) {
	var stats retentionStats
	if b.store == nil || b.maint.auditMaxAge <= 0 {
		return stats, nil
	}
	b.maint.passMu.Lock()
	defer b.maint.passMu.Unlock()
	started := time.Now()
	cutoff := b.clock.Now().Add(-b.maint.auditMaxAge).UnixNano()

	recs, err := b.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, kindHead)
	if err != nil {
		return stats, fmt.Errorf("durable: retention: scan head records: %w", err)
	}
	heads := recs[:0]
	for _, rec := range recs {
		if rec.Kind == kindHead {
			heads = append(heads, rec)
		}
	}
	b.retentionHeads.Store(int64(len(heads)))
	if len(heads) > retentionHeadsWarn {
		b.logger.WarnContext(ctx, "durable: audit retention traverses a very large head set; each pass reads every session header",
			slog.String("driver", "durable"), slog.Int("heads", len(heads)))
	}
	fences, err := b.loadDurableFenceSnapshot(ctx)
	if err != nil {
		return stats, fmt.Errorf("durable: retention: %w", err)
	}
	sort.Slice(heads, func(i, j int) bool { return sessionMapKey(heads[i].Identity) < sessionMapKey(heads[j].Identity) })
	startAt := 0
	if b.maint.resumeKey != "" {
		startAt = sort.Search(len(heads), func(i int) bool { return sessionMapKey(heads[i].Identity) >= b.maint.resumeKey })
		if startAt == len(heads) {
			startAt = 0
		}
	}
	budget := &deleteBudget{remaining: b.maint.deleteBudget, interval: b.maint.deleteInterval}
	horizonBefore := b.loadHorizon()
	horizonExact := true
	var oldest int64
	deferred := false
	for n := 0; n < len(heads); n++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rec := heads[(startAt+n)%len(heads)]
		stats.SessionsScanned++
		if !isIndexV2(rec.Bytes) {
			horizonExact = false // legacy heads carry no oldest field
			continue
		}
		if fences.contains(rec.Identity) {
			continue
		}
		hdr, err := decodeHeader(rec.Bytes)
		if err != nil {
			_ = b.noteIntegrity(ctx, rec.Identity, err)
			horizonExact = false
			continue
		}
		if hdr.RetainedCount > 0 && hdr.OldestOccurredAt < cutoff {
			trimmed, next, err := b.trimSessionFresh(ctx, rec.Identity, cutoff, &stats)
			switch {
			case err != nil && ctx.Err() != nil:
				return stats, ctx.Err()
			case err != nil:
				if errors.Is(err, ErrIndexIntegrity) {
					_ = b.noteIntegrity(ctx, rec.Identity, err)
				} else {
					b.logger.WarnContext(ctx, "durable: audit retention skipped a session this pass",
						slog.String("driver", "durable"),
						slog.String("head_identity_hash", headIdentityHash(rec.Identity)),
						slog.String("error", err.Error()))
				}
			case trimmed:
				hdr = next
			}
		}
		if hdr.Trimmed.CleanupPending && budget.remaining <= 0 {
			// Out of deletes: keep traversing so every other session still
			// gets its (delete-free) trim commit, and resume cleanup here.
			if !deferred {
				b.maint.resumeKey = sessionMapKey(rec.Identity)
				deferred = true
			}
			stats.DeletesDeferred = true
		} else if hdr.Trimmed.CleanupPending {
			budget.beginSession(b.maint.deleteSlice)
			if err := b.cleanupSession(ctx, rec.Identity, budget, &stats); err != nil {
				if errors.Is(err, errDeleteBudgetExhausted) || errors.Is(err, errSessionSliceExhausted) {
					if errors.Is(err, errDeleteBudgetExhausted) && !deferred {
						b.maint.resumeKey = sessionMapKey(rec.Identity)
						deferred = true
					}
					stats.DeletesDeferred = true
				} else if ctx.Err() != nil {
					return stats, ctx.Err()
				} else {
					b.logger.WarnContext(ctx, "durable: audit retention cleanup deferred for a session",
						slog.String("driver", "durable"),
						slog.String("head_identity_hash", headIdentityHash(rec.Identity)),
						slog.String("error", err.Error()))
				}
			}
		}
		if hdr.RetainedCount > 0 && (oldest == 0 || hdr.OldestOccurredAt < oldest) {
			oldest = hdr.OldestOccurredAt
		}
	}
	stats.Complete = true
	if !deferred {
		b.maint.resumeKey = ""
	}
	if horizonExact {
		// The horizon can move later once audit rows are trimmed. Only a
		// complete traversal over v2 headers knows the true minimum. A
		// publish that floored the horizon meanwhile wins this pass.
		b.replaceHorizonIf(horizonBefore, horizonSnapshot{nano: oldest, present: oldest != 0})
	}
	b.logger.InfoContext(ctx, "durable: audit retention pass",
		slog.String("driver", "durable"),
		slog.Int("sessions_scanned", stats.SessionsScanned),
		slog.Int("sessions_trimmed", stats.SessionsTrimmed),
		slog.Int("chunks_trimmed", stats.ChunksTrimmed),
		slog.Int("entries_deleted", stats.EntriesDeleted),
		slog.Bool("deletes_deferred", stats.DeletesDeferred),
		slog.Int("orphans_cleaned", stats.OrphansCleaned),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	return stats, nil
}

// trimSessionFresh re-reads the session header (the traversal snapshot may be
// minutes old on an active session) and trims it, retrying a bounded number of
// times when a concurrent publish wins the header CAS.
func (b *bus) trimSessionFresh(ctx context.Context, id identity.Quadruple, cutoff int64, stats *retentionStats) (bool, indexHeader, error) {
	for attempt := 1; attempt <= retentionTrimAttempts; attempt++ {
		rec, err := b.store.Load(ctx, id, kindHead)
		if errors.Is(err, state.ErrNotFound) {
			return false, indexHeader{}, nil
		}
		if err != nil {
			return false, indexHeader{}, fmt.Errorf("load head: %w", err)
		}
		if !isIndexV2(rec.Bytes) {
			return false, indexHeader{}, nil
		}
		hdr, err := decodeHeader(rec.Bytes)
		if err != nil {
			return false, indexHeader{}, err
		}
		trimmed, next, err := b.trimSession(ctx, rec, hdr, cutoff, stats)
		if errors.Is(err, errTrimConflict) {
			continue
		}
		if err != nil || trimmed {
			return trimmed, next, err
		}
		return false, hdr, nil
	}
	return false, indexHeader{}, nil
}

// retentionTrimAttempts bounds header-CAS retries for one session per pass.
const retentionTrimAttempts = 3

// errTrimConflict reports that the trim commit lost its header CAS.
var errTrimConflict = errors.New("durable: retention trim raced a concurrent header update")

// trimSession trims whole qualifying chunks from the front (prefix rule) and,
// when no sealed chunk remains, a fully qualifying tail (tail rule). The chain
// from base_digest through the cut is verified first; any mismatch aborts the
// session with ErrIndexIntegrity. The header and summary commit atomically; a
// CAS conflict (a publish or erasure raced) skips the session this pass.
func (b *bus) trimSession(ctx context.Context, rec state.StateRecord, hdr indexHeader, cutoff int64, stats *retentionStats) (bool, indexHeader, error) {
	id := rec.Identity
	prev := hdr.BaseDigest
	var trimmed []indexChunk
	var boundary *indexChunk
	for k := hdr.FirstChunk; k < hdr.NextChunk; k++ {
		if err := ctx.Err(); err != nil {
			return false, hdr, err
		}
		chunkRec, err := b.store.Load(ctx, id, chunkKind(k))
		if errors.Is(err, state.ErrNotFound) {
			// Another sweeper (a peer replica, or a deploy overlap) may have
			// trimmed and deleted it after this header was read.
			if current, loadErr := b.store.Load(ctx, id, kindHead); loadErr != nil || current.ID != rec.ID {
				return false, hdr, errTrimConflict
			}
			return false, hdr, integrityErrorf("header references chunk %d but it is missing", k)
		}
		if err != nil {
			return false, hdr, fmt.Errorf("load chunk %d: %w", k, err)
		}
		c, err := decodeChunk(chunkRec.Bytes, k)
		if err != nil {
			return false, hdr, err
		}
		if c.Prev != prev {
			return false, hdr, integrityErrorf("chunk %d does not link to its predecessor", k)
		}
		if k == hdr.FirstChunk && c.FirstSeq != hdr.FirstSeq {
			return false, hdr, integrityErrorf("oldest chunk %d starts at seq=%d, header first_seq=%d", k, c.FirstSeq, hdr.FirstSeq)
		}
		if len(trimmed) > 0 && c.FirstSeq <= trimmed[len(trimmed)-1].LastSeq {
			return false, hdr, integrityErrorf("chunk %d overlaps its predecessor", k)
		}
		if len(trimmed) >= retentionMaxChunksPerSession || !allQualify(c.Rows, cutoff) {
			boundary = &c
			break
		}
		trimmed = append(trimmed, c)
		prev = c.Digest
	}
	if boundary == nil && len(trimmed) > 0 {
		// Every sealed chunk is trimmed: the last one must be the header's
		// sealed chunk.
		if prev != hdr.SealedDigest {
			return false, hdr, integrityErrorf("trimmed chain does not end at the header's sealed digest")
		}
		last := trimmed[len(trimmed)-1]
		if len(hdr.Tail) == 0 && last.LastSeq != hdr.LastSeq {
			return false, hdr, integrityErrorf("newest chunk ends at seq=%d, header last_seq=%d", last.LastSeq, hdr.LastSeq)
		}
		if len(hdr.Tail) > 0 && hdr.Tail[0].Seq <= last.LastSeq {
			return false, hdr, integrityErrorf("tail overlaps the newest chunk")
		}
	}
	tailTrim := boundary == nil && len(hdr.Trimmed.PendingEntries) == 0 && allQualify(hdr.Tail, cutoff)
	if len(trimmed) == 0 && !tailTrim {
		return false, hdr, nil
	}

	next := hdr
	next.Tail = append([]indexRow(nil), hdr.Tail...)
	next.Trimmed.PendingEntries = append([]uint64(nil), hdr.Trimmed.PendingEntries...)
	var rows []indexRow
	for _, c := range trimmed {
		rows = append(rows, c.Rows...)
	}
	next.FirstChunk += uint64(len(trimmed))
	next.BaseDigest = prev
	if tailTrim {
		rows = append(rows, next.Tail...)
		for _, r := range next.Tail {
			next.Trimmed.PendingEntries = append(next.Trimmed.PendingEntries, r.Seq)
		}
		next.Tail = nil
	}
	next.Trimmed.Count += uint64(len(rows))
	next.Trimmed.ThroughSeq = rows[len(rows)-1].Seq
	if !hdr.Trimmed.CleanupPending {
		next.Trimmed.CleanupFrom = hdr.FirstChunk
	}
	next.Trimmed.CleanupPending = true
	next.RetainedCount -= uint64(len(rows))
	switch {
	case next.RetainedCount == 0:
		next.FirstSeq, next.OldestOccurredAt = 0, 0
	case boundary != nil:
		next.FirstSeq = boundary.FirstSeq
		next.OldestOccurredAt = boundary.MinOccurredAt
		for _, r := range next.Tail {
			next.OldestOccurredAt = min(next.OldestOccurredAt, r.OccurredAt)
		}
	default:
		next.FirstSeq = next.Tail[0].Seq
		next.OldestOccurredAt = next.Tail[0].OccurredAt
		for _, r := range next.Tail {
			next.OldestOccurredAt = min(next.OldestOccurredAt, r.OccurredAt)
		}
	}
	headerBytes, err := encodeHeader(next)
	if err != nil {
		return false, hdr, err
	}

	summary, summaryExpectation, err := b.loadRetentionSummary(ctx, id)
	if err != nil {
		return false, hdr, err
	}
	summary.add(rows, next.BaseDigest, b.maint.auditMaxAge, b.clock.Now())
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		return false, hdr, fmt.Errorf("encode retention summary: %w", err)
	}
	err = b.store.SaveBatchIf(ctx,
		[]state.SlotExpectation{{Identity: id, Kind: kindHead, ExpectedEventID: rec.ID}, summaryExpectation},
		[]state.StateRecord{
			{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headerBytes},
			{ID: state.NewEventID(), Identity: id, Kind: kindRetentionSummary, Bytes: summaryBytes},
		})
	switch {
	case err == nil:
	case errors.Is(err, state.ErrConditionFailed):
		// A publish, erasure, or another sweeper raced: re-read and retry.
		return false, hdr, errTrimConflict
	case errors.Is(err, state.ErrCommitOutcomeUnknown):
		// The next pass re-reads the header: a landed trim is then cleaned
		// up, an absent one is retried.
		return false, hdr, nil
	default:
		return false, hdr, fmt.Errorf("commit trim: %w", err)
	}
	stats.SessionsTrimmed++
	stats.ChunksTrimmed += len(trimmed)
	stats.RowsTrimmed += len(rows)
	return true, next, nil
}

func (b *bus) loadRetentionSummary(ctx context.Context, id identity.Quadruple) (retentionSummary, state.SlotExpectation, error) {
	expectation := state.SlotExpectation{Identity: id, Kind: kindRetentionSummary}
	rec, err := b.store.Load(ctx, id, kindRetentionSummary)
	if errors.Is(err, state.ErrNotFound) {
		return retentionSummary{Format: 1, ByType: map[string]uint64{}}, expectation, nil
	}
	if err != nil {
		return retentionSummary{}, expectation, fmt.Errorf("load retention summary: %w", err)
	}
	var s retentionSummary
	if err := strictUnmarshal(rec.Bytes, &s); err != nil || s.Format != 1 {
		return retentionSummary{}, expectation, integrityErrorf("retention summary is malformed")
	}
	if s.ByType == nil {
		s.ByType = map[string]uint64{}
	}
	expectation.ExpectedEventID = rec.ID
	return s, expectation, nil
}

func (s *retentionSummary) add(rows []indexRow, anchor string, maxAge time.Duration, now time.Time) {
	for _, r := range rows {
		s.TrimmedCount++
		s.ByType[string(r.Type)]++
		if s.FirstTrimmedSeq == 0 {
			s.FirstTrimmedSeq = r.Seq
		}
		s.LastTrimmedSeq = r.Seq
		if s.FirstTrimmedOccurredAt == 0 || r.OccurredAt < s.FirstTrimmedOccurredAt {
			s.FirstTrimmedOccurredAt = r.OccurredAt
		}
		if r.OccurredAt > s.LastTrimmedOccurredAt {
			s.LastTrimmedOccurredAt = r.OccurredAt
		}
	}
	s.ChainAnchor = anchor
	s.Policy = retentionSummaryPolicy{AuditMaxAge: maxAge.String()}
	s.LastPassAt = now.UnixNano()
	s.Passes++
}

// cleanupSession removes trimmed records still present in storage. The
// header's trimmed.cleanup_from names the lowest chunk index whose records may
// remain, so cleanup walks the explicit indices [cleanup_from, first_chunk) —
// each chunk's entries first, then the chunk — plus the entries of a trimmed
// tail; it never infers exhaustion from a bounded listing. Every delete
// re-checks that the record lies at or below the header's trimmed
// through_seq: sequences are global and never reused, so a session erased and
// recreated meanwhile is never touched. Progress is saved by header CAS; when
// nothing remains the cleanup marker is cleared.
func (b *bus) cleanupSession(ctx context.Context, id identity.Quadruple, budget *deleteBudget, stats *retentionStats) error {
	rec, err := b.store.Load(ctx, id, kindHead)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load head: %w", err)
	}
	if !isIndexV2(rec.Bytes) {
		return nil
	}
	hdr, err := decodeHeader(rec.Bytes)
	if err != nil {
		return err
	}
	if !hdr.Trimmed.CleanupPending {
		return nil
	}
	through := hdr.Trimmed.ThroughSeq
	next := hdr.Trimmed.CleanupFrom
	var stopErr error
	blocked := false // a chunk that cannot be verified stays for an operator
	for ; next < hdr.FirstChunk && stopErr == nil; next++ {
		chunkRec, err := b.store.Load(ctx, id, chunkKind(next))
		if errors.Is(err, state.ErrNotFound) {
			continue // entries are deleted before their chunk record
		}
		if err != nil {
			return fmt.Errorf("load trimmed chunk %d: %w", next, err)
		}
		c, err := decodeChunk(chunkRec.Bytes, next)
		if err != nil || c.LastSeq > through {
			b.logger.WarnContext(ctx, "durable: audit retention cannot verify a trimmed chunk; leaving it in place",
				slog.String("driver", "durable"),
				slog.String("head_identity_hash", headIdentityHash(id)),
				slog.Uint64("chunk", next))
			blocked = true
			break
		}
		for _, r := range c.Rows {
			if err := b.deleteTrimmedEntry(ctx, id, r.Seq, budget, stats); err != nil {
				stopErr = err
				break
			}
		}
		if stopErr != nil {
			break
		}
		if err := budget.take(ctx); err != nil {
			stopErr = err
			break
		}
		if _, err := b.store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: chunkRec.Kind, ExpectedEventID: chunkRec.ID}); err != nil {
			return fmt.Errorf("delete trimmed chunk %d: %w", next, err)
		}
		stats.ChunksDeleted++
		stats.OrphansCleaned++
	}
	pendingDone := false
	if stopErr == nil && !blocked && next >= hdr.FirstChunk {
		pendingDone = true
		for _, seq := range hdr.Trimmed.PendingEntries {
			if seq > through {
				continue
			}
			if err := b.deleteTrimmedEntry(ctx, id, seq, budget, stats); err != nil {
				stopErr = err
				pendingDone = false
				break
			}
		}
	}
	if next == hdr.Trimmed.CleanupFrom && !pendingDone {
		return stopErr // no progress to record
	}
	progressed := hdr
	progressed.Trimmed.CleanupFrom = min(next, hdr.FirstChunk)
	if pendingDone {
		progressed.Trimmed.CleanupPending = false
		progressed.Trimmed.PendingEntries = nil
		progressed.Trimmed.CleanupFrom = 0
	}
	headerBytes, err := encodeHeader(progressed)
	if err != nil {
		return err
	}
	err = b.store.SaveIf(ctx, []state.SlotExpectation{{Identity: id, Kind: kindHead, ExpectedEventID: rec.ID}},
		state.StateRecord{ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: headerBytes})
	if err != nil && !errors.Is(err, state.ErrConditionFailed) && !errors.Is(err, state.ErrCommitOutcomeUnknown) {
		return fmt.Errorf("save cleanup progress: %w", err)
	}
	// A conflict leaves the old progress; the next pass repeats idempotently.
	return stopErr
}

func (b *bus) deleteTrimmedEntry(ctx context.Context, id identity.Quadruple, seq uint64, budget *deleteBudget, stats *retentionStats) error {
	kind := kindEntryPrefix + seqToken(seq)
	rec, err := b.store.Load(ctx, id, kind)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load trimmed entry seq=%d: %w", seq, err)
	}
	if err := budget.take(ctx); err != nil {
		return err
	}
	if _, err := b.store.DeleteIf(ctx, state.SlotExpectation{Identity: id, Kind: kind, ExpectedEventID: rec.ID}); err != nil {
		return fmt.Errorf("delete trimmed entry seq=%d: %w", seq, err)
	}
	stats.EntriesDeleted++
	return nil
}
