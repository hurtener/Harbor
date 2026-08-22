package durable

// This file owns the offline, fail-closed repair contract for legacy durable
// heads.  It intentionally operates only through state.StateStore, so the
// same inspection and CAS semantics apply to the in-memory, SQLite, and
// PostgreSQL drivers.  It never changes an immutable entry body: the only
// write is a conditional replacement of one mutable head plus its protected
// content-free receipt.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	// LegacyRepairToolVersion is embedded in receipts so operators can
	// distinguish repair receipts produced by different contracts.
	LegacyRepairToolVersion     = "v1.29.3"
	legacyRepairReceiptPrefix   = state.InternalKindPrefix + "events/legacy-head-repair/"
	legacyRepairDefaultAttempts = 8
	legacyRepairDefaultMaxHeads = state.MaxStateMaintenanceListLimit - 1
)

// LegacyRepairMode selects whether the offline command only inspects, applies
// validated repairs, or verifies that no duplicate references remain.
type LegacyRepairMode string

const (
	LegacyRepairInspect LegacyRepairMode = "inspect"
	LegacyRepairApply   LegacyRepairMode = "apply"
	LegacyRepairVerify  LegacyRepairMode = "verify"
)

var (
	// ErrLegacyRepairRefused means at least one head failed the strict
	// repairability contract. No write is attempted for that inspection.
	ErrLegacyRepairRefused = errors.New("durable: legacy head repair refused")
	// ErrLegacyRepairPending means verify found a still-duplicated head.
	ErrLegacyRepairPending = errors.New("durable: legacy head repair still pending")
)

// LegacyRepairOptions controls one offline repair run. Apply requires
// WriterDrained to be true; this is deliberately an in-process acknowledgement
// rather than an inferred health signal.
type LegacyRepairOptions struct {
	Mode          LegacyRepairMode
	WriterDrained bool
	ToolVersion   string
	MaxAttempts   int
	MaxHeads      int
	MaxDuplicates int
}

// LegacyRepairDuplicate is a content-free description of one duplicated
// sequence. Positions are zero-based offsets in the legacy head. EntryHash and
// MetadataHash identify bytes without exposing bodies or identity values.
type LegacyRepairDuplicate struct {
	Sequence     uint64           `json:"sequence"`
	Positions    []int            `json:"positions"`
	EntryHash    string           `json:"entry_hash_sha256"`
	MetadataHash string           `json:"payload_metadata_hash_sha256"`
	PayloadType  events.EventType `json:"payload_type"`
	PayloadSeq   uint64           `json:"payload_sequence"`
}

// LegacyRepairHead is the content-free result for one durable head.
type LegacyRepairHead struct {
	HeadIdentityHash   string                  `json:"head_identity_hash_sha256"`
	BeforeHeadHash     string                  `json:"before_head_hash_sha256"`
	AfterHeadHash      string                  `json:"after_head_hash_sha256,omitempty"`
	ExpectedGeneration string                  `json:"expected_generation,omitempty"`
	AppliedGeneration  string                  `json:"applied_generation,omitempty"`
	Duplicates         []LegacyRepairDuplicate `json:"duplicates,omitempty"`
	Outcome            string                  `json:"outcome"`
	ReceiptID          string                  `json:"receipt_id,omitempty"`
}

// LegacyRepairReceipt is persisted atomically beside the repaired head. It
// contains no payload bytes and no raw identity values. Its exact JSON bytes
// are returned on response-loss retries.
type LegacyRepairReceipt struct {
	ReceiptVersion     int                     `json:"receipt_version"`
	ReceiptID          string                  `json:"receipt_id"`
	ToolVersion        string                  `json:"tool_version"`
	HeadIdentityHash   string                  `json:"head_identity_hash_sha256"`
	BeforeHeadHash     string                  `json:"before_head_hash_sha256"`
	AfterHeadHash      string                  `json:"after_head_hash_sha256"`
	ExpectedGeneration string                  `json:"expected_generation"`
	AppliedGeneration  string                  `json:"applied_generation"`
	Duplicates         []LegacyRepairDuplicate `json:"duplicates"`
	Outcome            string                  `json:"outcome"`
}

// LegacyRepairReport is the deterministic content-free output of one scan.
type LegacyRepairReport struct {
	ToolVersion             string             `json:"tool_version"`
	Mode                    LegacyRepairMode   `json:"mode"`
	HeadsScanned            int                `json:"heads_scanned"`
	AffectedHeadCount       int                `json:"affected_head_count"`
	DuplicateSequenceCount  int                `json:"duplicate_sequence_count"`
	RedundantReferenceCount int                `json:"redundant_reference_count"`
	Heads                   []LegacyRepairHead `json:"heads,omitempty"`
}

type legacyRepairHeadView struct {
	record     state.StateRecord
	head       headRecord
	duplicates []LegacyRepairDuplicate
}

// ValidateLegacyRepairApplyDSN rejects known transaction-pooled PostgreSQL
// endpoints for mutation. Inspection may use a read-only pool, but apply must
// use a direct, session-affine PostgreSQL connection.
func ValidateLegacyRepairApplyDSN(dsn string) error {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return errors.New("legacy head repair: direct apply DSN is required")
	}
	lower := strings.ToLower(trimmed)
	compact := strings.Join(strings.Fields(lower), "")
	if strings.Contains(compact, "pgbouncer_mode=transaction") || strings.Contains(compact, "pool_mode=transaction") {
		return errors.New("legacy head repair: apply refuses transaction-pooled DSN; use direct PostgreSQL 5432, never PgBouncer 6432")
	}
	if strings.Contains(compact, ":6432") || strings.Contains(compact, "port=6432") {
		return errors.New("legacy head repair: apply DSN resolves to PgBouncer 6432; use a direct PostgreSQL 5432 endpoint")
	}
	// URL parsing catches an encoded/structured port even when the textual
	// endpoint did not contain the usual ":6432" spelling.
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" {
		if parsed.Port() != "5432" {
			return errors.New("legacy head repair: apply requires an explicit direct PostgreSQL port=5432 endpoint")
		}
		return nil
	}
	if !strings.Contains(compact, "port=5432") {
		return errors.New("legacy head repair: apply requires an explicit direct PostgreSQL port=5432 endpoint")
	}
	return nil
}

// InspectLegacyHeads performs a complete, cancellable, content-free scan.
// It never writes. A malformed or ambiguous head returns ErrLegacyRepairRefused
// before any result is considered repairable.
func InspectLegacyHeads(ctx context.Context, store state.StateStore) (LegacyRepairReport, error) {
	return runLegacyRepair(ctx, store, LegacyRepairOptions{Mode: LegacyRepairInspect})
}

// RepairLegacyHeads applies every validated duplicate repair after requiring
// the explicit writer-drained acknowledgement. It retries only a clean
// re-read after a conditional generation conflict.
func RepairLegacyHeads(ctx context.Context, store state.StateStore, opts LegacyRepairOptions) (LegacyRepairReport, error) {
	if opts.Mode == "" {
		opts.Mode = LegacyRepairInspect
	}
	return runLegacyRepair(ctx, store, opts)
}

func runLegacyRepair(ctx context.Context, store state.StateStore, opts LegacyRepairOptions) (LegacyRepairReport, error) {
	if store == nil {
		return LegacyRepairReport{}, fmt.Errorf("%w: state store is required", ErrLegacyRepairRefused)
	}
	if opts.Mode != LegacyRepairInspect && opts.Mode != LegacyRepairApply && opts.Mode != LegacyRepairVerify {
		return LegacyRepairReport{}, fmt.Errorf("%w: unknown mode %q", ErrLegacyRepairRefused, opts.Mode)
	}
	if opts.Mode == LegacyRepairApply && !opts.WriterDrained {
		return LegacyRepairReport{}, fmt.Errorf("%w: apply requires explicit writer-drained/frozen acknowledgement", ErrLegacyRepairRefused)
	}
	if opts.ToolVersion == "" {
		opts.ToolVersion = LegacyRepairToolVersion
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = legacyRepairDefaultAttempts
	}
	if opts.MaxHeads <= 0 {
		opts.MaxHeads = legacyRepairDefaultMaxHeads
	}
	if opts.MaxHeads >= state.MaxStateMaintenanceListLimit {
		return LegacyRepairReport{}, fmt.Errorf("%w: max heads %d must be below storage maintenance bound %d", ErrLegacyRepairRefused, opts.MaxHeads, state.MaxStateMaintenanceListLimit)
	}

	views, err := scanLegacyRepairHeads(ctx, store, opts.MaxHeads)
	if err != nil {
		return LegacyRepairReport{}, err
	}
	if opts.MaxHeads > 0 && len(views) > opts.MaxHeads {
		return LegacyRepairReport{}, fmt.Errorf("%w: head scan found %d records, above configured limit %d", ErrLegacyRepairRefused, len(views), opts.MaxHeads)
	}
	report := buildLegacyRepairReport(opts.Mode, opts.ToolVersion, views)
	if opts.MaxDuplicates > 0 && report.DuplicateSequenceCount > opts.MaxDuplicates {
		return LegacyRepairReport{}, fmt.Errorf("%w: scan found %d duplicate sequences, above configured limit %d", ErrLegacyRepairRefused, report.DuplicateSequenceCount, opts.MaxDuplicates)
	}
	if opts.Mode == LegacyRepairInspect {
		return report, nil
	}
	if opts.Mode == LegacyRepairVerify {
		if report.AffectedHeadCount != 0 {
			return report, fmt.Errorf("%w: %d head(s) still contain duplicate references", ErrLegacyRepairPending, report.AffectedHeadCount)
		}
		// A canonical head may have a persisted repair receipt from an earlier
		// apply. Validate every receipt that is present even when no duplicate
		// remains; verify is the operator's post-apply proof boundary and must
		// reject stale or forged evidence rather than reporting a clean scan.
		for _, view := range views {
			if _, _, err := loadLegacyRepairReceipt(ctx, store, view.record, opts.ToolVersion); err != nil {
				return report, err
			}
		}
		return report, nil
	}

	// The full preflight scan above completes before any write. This prevents a
	// known ambiguous head later in the store from being discovered after an
	// earlier head was already changed.
	for _, view := range views {
		if len(view.duplicates) == 0 {
			continue
		}
		if err := applyLegacyRepairHead(ctx, store, view, opts); err != nil {
			return report, err
		}
	}
	// Re-scan after all CAS operations. This proves no duplicate remains and
	// returns the exact persisted receipt on response-loss/idempotent replay.
	postViews, err := scanLegacyRepairHeads(ctx, store, opts.MaxHeads)
	if err != nil {
		return report, err
	}
	return buildAppliedLegacyRepairReport(ctx, store, opts.ToolVersion, views, postViews)
}

func scanLegacyRepairHeads(ctx context.Context, store state.StateStore, maxHeads int) ([]legacyRepairHeadView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxHeads < 1 || maxHeads >= state.MaxStateMaintenanceListLimit {
		return nil, fmt.Errorf("%w: max heads %d is outside bounded maintenance scan capacity", ErrLegacyRepairRefused, maxHeads)
	}
	records, err := store.ListKindBounded(ctx, state.ListScope{MaintenanceScoped: true}, kindHead, maxHeads+1)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: head scan failed", ErrLegacyRepairRefused)
	}
	if len(records) > maxHeads {
		return nil, fmt.Errorf("%w: head scan found more than configured limit %d", ErrLegacyRepairRefused, maxHeads)
	}
	views := make([]legacyRepairHeadView, 0, len(records))
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rec.Kind != kindHead {
			continue
		}
		view, err := inspectLegacyRepairHead(ctx, store, rec)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		left := headIdentityHash(views[i].record.Identity)
		right := headIdentityHash(views[j].record.Identity)
		if left != right {
			return left < right
		}
		return views[i].record.ID < views[j].record.ID
	})
	return views, nil
}

func inspectLegacyRepairHead(ctx context.Context, store state.StateStore, rec state.StateRecord) (legacyRepairHeadView, error) {
	identityHash := headIdentityHash(rec.Identity)
	refuse := func(reason string) (legacyRepairHeadView, error) {
		return legacyRepairHeadView{}, fmt.Errorf("%w: head=%s: %s", ErrLegacyRepairRefused, identityHash, reason)
	}
	if err := state.ValidateIdentity(rec.Identity); err != nil {
		return refuse("incomplete storage identity")
	}
	if rec.ID == "" {
		return refuse("head storage generation is empty")
	}
	if rec.Kind != kindHead {
		return refuse("head storage kind is not canonical")
	}
	if rec.Identity.RunID != "" {
		return refuse("head storage RunID must be empty for a session-scoped durable head")
	}
	head, err := decodeHead(rec.Bytes)
	if err != nil {
		return refuse("malformed head")
	}
	if len(head.Sequences) == 0 {
		if len(head.Metadata) != 0 || head.MetadataReady || head.MetadataValidatedCount != 0 || head.MetadataIntegrityChecksum != "" {
			return refuse("empty head carries metadata")
		}
		return legacyRepairHeadView{record: rec, head: head}, nil
	}

	positions := make(map[uint64][]int)
	var previous uint64
	for i, seq := range head.Sequences {
		if err := ctx.Err(); err != nil {
			return legacyRepairHeadView{}, err
		}
		if seq == 0 {
			return refuse("head contains zero sequence")
		}
		if i > 0 && seq < previous {
			return refuse("head sequence order is not canonical")
		}
		positions[seq] = append(positions[seq], i)
		previous = seq
	}

	if head.MetadataReady {
		if !headMetadataReady(head) {
			return refuse("metadata-ready head has invalid length or integrity checksum")
		}
	}
	if len(head.Metadata) > 0 {
		if len(head.Metadata) != len(head.Sequences) {
			return refuse("head metadata is partial and cannot be repaired safely")
		}
		for _, m := range head.Metadata {
			if err := validateLegacyRepairMetadata(m, rec.Identity, positions); err != nil {
				return refuse(err.Error())
			}
		}
	} else if head.MetadataReady || head.MetadataValidatedCount != 0 || head.MetadataIntegrityChecksum != "" {
		return refuse("head metadata bookkeeping is inconsistent")
	}

	duplicates := make([]LegacyRepairDuplicate, 0)
	for seq, pos := range positions {
		if len(pos) < 2 {
			continue
		}
		for i := 1; i < len(pos); i++ {
			if pos[i] != pos[i-1]+1 {
				return refuse(fmt.Sprintf("sequence=%d duplicate positions are non-adjacent", seq))
			}
		}
		entryKind := kindEntryPrefix + seqToken(seq)
		entry, loadErr := store.Load(ctx, rec.Identity, entryKind)
		if loadErr != nil {
			if isLegacyRepairContextError(loadErr) {
				return legacyRepairHeadView{}, loadErr
			}
			return refuse(fmt.Sprintf("sequence=%d immutable entry is unavailable", seq))
		}
		if entry.Identity != rec.Identity || entry.Kind != entryKind {
			return refuse(fmt.Sprintf("sequence=%d entry storage identity/kind mismatch", seq))
		}
		ev, decodeErr := decodeEvent(entry.Bytes)
		if decodeErr != nil {
			return refuse(fmt.Sprintf("sequence=%d immutable entry body is malformed", seq))
		}
		if ev.Sequence != seq || ev.Identity.Identity != rec.Identity.Identity {
			return refuse(fmt.Sprintf("sequence=%d payload sequence or identity mismatch", seq))
		}
		if !events.IsValidEventType(ev.Type) {
			return refuse(fmt.Sprintf("sequence=%d payload event type is unknown", seq))
		}
		canonical, metadataErr := metadataRecordFromEvent(ev)
		if metadataErr != nil {
			return refuse(fmt.Sprintf("sequence=%d payload metadata is malformed", seq))
		}
		if len(head.Metadata) > 0 {
			for _, index := range pos {
				if head.Metadata[index] != canonical {
					return refuse(fmt.Sprintf("sequence=%d duplicate metadata projection conflicts with immutable body", seq))
				}
			}
		}
		duplicates = append(duplicates, LegacyRepairDuplicate{
			Sequence:     seq,
			Positions:    append([]int(nil), pos...),
			EntryHash:    sha256Hex(entry.Bytes),
			MetadataHash: sha256JSON(canonical),
			PayloadType:  ev.Type,
			PayloadSeq:   ev.Sequence,
		})
	}
	sort.Slice(duplicates, func(i, j int) bool { return duplicates[i].Sequence < duplicates[j].Sequence })
	return legacyRepairHeadView{record: rec, head: head, duplicates: duplicates}, nil
}

func validateLegacyRepairMetadata(m eventMetadataRecord, id identity.Quadruple, sequences map[uint64][]int) error {
	if m.Sequence == 0 || m.Type == "" || !events.IsValidEventType(m.Type) {
		return fmt.Errorf("metadata has invalid sequence or event type")
	}
	if _, ok := sequences[m.Sequence]; !ok {
		return fmt.Errorf("metadata sequence is not present in head")
	}
	if m.TenantID != id.TenantID || m.UserID != id.UserID || m.SessionID != id.SessionID {
		return fmt.Errorf("metadata identity does not match head storage identity")
	}
	if m.Internal != events.IsBusInternalNotice(m.Type) {
		return fmt.Errorf("metadata internal marker disagrees with event type")
	}
	return nil
}

func applyLegacyRepairHead(ctx context.Context, store state.StateStore, view legacyRepairHeadView, opts LegacyRepairOptions) error {
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := store.Load(ctx, view.record.Identity, kindHead)
		if err != nil {
			if isLegacyRepairContextError(err) {
				return err
			}
			return fmt.Errorf("%w: head=%s: clean re-read failed", ErrLegacyRepairRefused, headIdentityHash(view.record.Identity))
		}
		fresh, err := inspectLegacyRepairHead(ctx, store, current)
		if err != nil {
			return err
		}
		_, ok, receiptErr := loadLegacyRepairReceipt(ctx, store, current, opts.ToolVersion)
		if receiptErr != nil {
			// A concurrent repair may have committed the head and receipt
			// between the clean head read above and this receipt read. Re-read
			// both slots before refusing; a malformed or stale receipt that
			// persists on the current head still fails closed below.
			latest, latestErr := store.Load(ctx, view.record.Identity, kindHead)
			if latestErr == nil {
				latestView, latestViewErr := inspectLegacyRepairHead(ctx, store, latest)
				if latestViewErr == nil && len(latestView.duplicates) == 0 {
					if _, latestOK, latestReceiptErr := loadLegacyRepairReceipt(ctx, store, latest, opts.ToolVersion); latestReceiptErr == nil && latestOK {
						return nil
					}
				}
			}
			return receiptErr
		}
		if ok {
			if len(fresh.duplicates) == 0 {
				return nil
			}
			return fmt.Errorf("%w: head=%s: persisted receipt is stale while duplicate references remain", ErrLegacyRepairRefused, headIdentityHash(current.Identity))
		}
		if len(fresh.duplicates) == 0 {
			return nil
		}
		newHead := fresh.head
		newHead.Sequences = removeLegacyDuplicateReferences(newHead.Sequences, fresh.duplicates)
		if len(newHead.Metadata) > 0 {
			newHead.Metadata = removeLegacyDuplicateMetadata(newHead.Metadata, fresh.duplicates)
			newHead.MetadataValidatedCount = len(newHead.Metadata)
			newHead.MetadataIntegrityChecksum = metadataIntegrityChecksum(newHead.Sequences, newHead.Metadata)
			if newHead.MetadataReady {
				newHead.MetadataValidatedCount = len(newHead.Sequences)
			}
		}
		newHeadBytes, encodeErr := encodeHead(newHead)
		if encodeErr != nil {
			return fmt.Errorf("%w: head=%s: repaired head encoding failed", ErrLegacyRepairRefused, headIdentityHash(current.Identity))
		}
		nextHeadID := state.NewEventID()
		receiptID := state.NewEventID()
		repairReceipt := LegacyRepairReceipt{
			ReceiptVersion:     1,
			ReceiptID:          string(receiptID),
			ToolVersion:        opts.ToolVersion,
			HeadIdentityHash:   headIdentityHash(current.Identity),
			BeforeHeadHash:     sha256Hex(current.Bytes),
			AfterHeadHash:      sha256Hex(newHeadBytes),
			ExpectedGeneration: string(current.ID),
			AppliedGeneration:  string(nextHeadID),
			Duplicates:         cloneLegacyRepairDuplicates(fresh.duplicates),
			Outcome:            "applied",
		}
		receiptBytes, marshalErr := json.Marshal(repairReceipt)
		if marshalErr != nil {
			return fmt.Errorf("%w: head=%s: receipt encoding failed", ErrLegacyRepairRefused, headIdentityHash(current.Identity))
		}
		nextHead := state.StateRecord{ID: nextHeadID, Identity: current.Identity, Kind: kindHead, Bytes: newHeadBytes}
		nextReceipt := state.NewInternalRecord(receiptID, current.Identity, legacyRepairReceiptKind(current.Identity), receiptBytes)
		err = store.SaveBatchIf(ctx,
			[]state.SlotExpectation{
				{Identity: current.Identity, Kind: kindHead, ExpectedEventID: current.ID},
				state.InternalSlotExpectation(current.Identity, legacyRepairReceiptKind(current.Identity), ""),
			},
			[]state.StateRecord{nextHead, nextReceipt})
		if err == nil {
			return nil
		}
		if isLegacyRepairContextError(err) && !errors.Is(err, state.ErrCommitOutcomeUnknown) {
			return err
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			// A commit acknowledgement may be unknown. The receipt is the
			// recovery mechanism; the caller can rerun safely, but no body is
			// ever rewritten here.
			return fmt.Errorf("%w: head=%s: conditional repair write failed: %w", ErrLegacyRepairRefused, headIdentityHash(current.Identity), err)
		}
	}
	return fmt.Errorf("%w: head=%s: generation changed repeatedly", ErrLegacyRepairRefused, headIdentityHash(view.record.Identity))
}

func removeLegacyDuplicateReferences(sequences []uint64, duplicates []LegacyRepairDuplicate) []uint64 {
	drop := make(map[int]struct{})
	for _, duplicate := range duplicates {
		for _, pos := range duplicate.Positions[1:] {
			drop[pos] = struct{}{}
		}
	}
	out := make([]uint64, 0, len(sequences)-len(drop))
	for i, seq := range sequences {
		if _, ok := drop[i]; !ok {
			out = append(out, seq)
		}
	}
	return out
}

func removeLegacyDuplicateMetadata(metadata []eventMetadataRecord, duplicates []LegacyRepairDuplicate) []eventMetadataRecord {
	drop := make(map[int]struct{})
	for _, duplicate := range duplicates {
		for _, pos := range duplicate.Positions[1:] {
			drop[pos] = struct{}{}
		}
	}
	out := make([]eventMetadataRecord, 0, len(metadata)-len(drop))
	for i, m := range metadata {
		if _, ok := drop[i]; !ok {
			out = append(out, m)
		}
	}
	return out
}

func loadLegacyRepairReceipt(ctx context.Context, store state.StateStore, head state.StateRecord, toolVersion string) (LegacyRepairReceipt, bool, error) {
	id := head.Identity
	rec, err := store.Load(ctx, id, legacyRepairReceiptKind(id))
	if errors.Is(err, state.ErrNotFound) {
		return LegacyRepairReceipt{}, false, nil
	}
	if err != nil {
		if isLegacyRepairContextError(err) {
			return LegacyRepairReceipt{}, false, err
		}
		return LegacyRepairReceipt{}, false, fmt.Errorf("%w: receipt read failed", ErrLegacyRepairRefused)
	}
	var receipt LegacyRepairReceipt
	if err := json.Unmarshal(rec.Bytes, &receipt); err != nil {
		return LegacyRepairReceipt{}, false, fmt.Errorf("%w: receipt is malformed or belongs to another head", ErrLegacyRepairRefused)
	}
	refuse := func(reason string) (LegacyRepairReceipt, bool, error) {
		return LegacyRepairReceipt{}, false, fmt.Errorf("%w: receipt %s", ErrLegacyRepairRefused, reason)
	}
	if head.Kind != kindHead {
		return refuse("head kind is not canonical")
	}
	if rec.Kind != legacyRepairReceiptKind(id) || rec.Identity != id {
		return refuse("record kind or identity is wrong")
	}
	if rec.ID == "" {
		return refuse("receipt record generation is empty")
	}
	if receipt.ReceiptVersion != 1 || receipt.ReceiptID != string(rec.ID) {
		return refuse("record identity does not match receipt ID")
	}
	if receipt.ToolVersion == "" || (toolVersion != "" && receipt.ToolVersion != toolVersion) {
		return refuse("tool version is missing or unexpected")
	}
	if receipt.HeadIdentityHash != headIdentityHash(id) {
		return refuse("head identity hash does not match")
	}
	if receipt.ExpectedGeneration == "" || receipt.AppliedGeneration == "" || receipt.ExpectedGeneration == receipt.AppliedGeneration {
		return refuse("receipt generations are invalid")
	}
	if receipt.AppliedGeneration != string(head.ID) {
		return refuse("applied generation does not match current head")
	}
	if receipt.Outcome != "applied" {
		return refuse("outcome is not applied")
	}
	if !validSHA256(receipt.BeforeHeadHash) || !validSHA256(receipt.AfterHeadHash) || receipt.BeforeHeadHash == receipt.AfterHeadHash {
		return refuse("receipt hashes are invalid")
	}
	if receipt.AfterHeadHash != sha256Hex(head.Bytes) {
		return refuse(fmt.Sprintf("after-head hash is stale expected=%s observed=%s", receipt.AfterHeadHash, sha256Hex(head.Bytes)))
	}
	if len(receipt.Duplicates) == 0 {
		return refuse("duplicate evidence is empty")
	}
	lastSequence := uint64(0)
	lastPosition := -1
	for _, duplicate := range receipt.Duplicates {
		if duplicate.Sequence == 0 || duplicate.Sequence < lastSequence ||
			duplicate.PayloadSeq != duplicate.Sequence || !events.IsValidEventType(duplicate.PayloadType) ||
			!validSHA256(duplicate.EntryHash) || !validSHA256(duplicate.MetadataHash) || len(duplicate.Positions) < 2 {
			return refuse("duplicate evidence is invalid")
		}
		for i, position := range duplicate.Positions {
			if position < 0 || position <= lastPosition || (i > 0 && position != duplicate.Positions[i-1]+1) {
				return refuse("duplicate positions are not canonical")
			}
			lastPosition = position
		}
		lastSequence = duplicate.Sequence
	}
	if err := validateLegacyRepairReceiptEvidence(ctx, store, head, receipt); err != nil {
		return LegacyRepairReceipt{}, false, err
	}
	return receipt, true, nil
}

// validateLegacyRepairReceiptEvidence rebinds the content-free receipt to the
// immutable entry rows that still exist after head canonicalisation. The
// current head hash/generation proves which mutable head the receipt belongs
// to; these entry checks prove that its sequence/type/body metadata evidence
// was not forged or made stale after the original apply.
func validateLegacyRepairReceiptEvidence(ctx context.Context, store state.StateStore, head state.StateRecord, receipt LegacyRepairReceipt) error {
	hd, err := decodeHead(head.Bytes)
	if err != nil {
		return fmt.Errorf("%w: current head is malformed while validating receipt", ErrLegacyRepairRefused)
	}
	currentSequences := make(map[uint64]struct{}, len(hd.Sequences))
	for _, seq := range hd.Sequences {
		currentSequences[seq] = struct{}{}
	}
	for _, duplicate := range receipt.Duplicates {
		if _, ok := currentSequences[duplicate.Sequence]; !ok {
			return fmt.Errorf("%w: receipt sequence=%d is absent from current head", ErrLegacyRepairRefused, duplicate.Sequence)
		}
		entryKind := kindEntryPrefix + seqToken(duplicate.Sequence)
		entry, err := store.Load(ctx, head.Identity, entryKind)
		if err != nil {
			if isLegacyRepairContextError(err) {
				return err
			}
			return fmt.Errorf("%w: receipt sequence=%d immutable entry is unavailable", ErrLegacyRepairRefused, duplicate.Sequence)
		}
		if entry.ID == "" || entry.Identity != head.Identity || entry.Kind != entryKind {
			return fmt.Errorf("%w: receipt sequence=%d immutable entry storage identity/kind mismatch", ErrLegacyRepairRefused, duplicate.Sequence)
		}
		ev, err := decodeEvent(entry.Bytes)
		if err != nil || ev.Sequence != duplicate.Sequence || ev.Identity.Identity != head.Identity.Identity || !events.IsValidEventType(ev.Type) {
			return fmt.Errorf("%w: receipt sequence=%d immutable entry payload mismatch", ErrLegacyRepairRefused, duplicate.Sequence)
		}
		canonical, err := metadataRecordFromEvent(ev)
		if err != nil || duplicate.PayloadType != ev.Type || duplicate.PayloadSeq != ev.Sequence || duplicate.EntryHash != sha256Hex(entry.Bytes) || duplicate.MetadataHash != sha256JSON(canonical) {
			return fmt.Errorf("%w: receipt sequence=%d immutable entry evidence mismatch", ErrLegacyRepairRefused, duplicate.Sequence)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isLegacyRepairContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func buildAppliedLegacyRepairReport(ctx context.Context, store state.StateStore, toolVersion string, before, after []legacyRepairHeadView) (LegacyRepairReport, error) {
	byIdentity := make(map[string]legacyRepairHeadView, len(after))
	for _, view := range after {
		byIdentity[headIdentityHash(view.record.Identity)] = view
	}
	report := LegacyRepairReport{ToolVersion: toolVersion, Mode: LegacyRepairApply, HeadsScanned: len(after)}
	for _, view := range before {
		identityHash := headIdentityHash(view.record.Identity)
		current, ok := byIdentity[identityHash]
		if !ok {
			continue
		}
		// Re-read the head before binding a receipt to it. Another repair
		// worker may have committed between the bounded post-scan and this
		// report assembly; never compare a receipt with an old head snapshot.
		latestRecord, err := store.Load(ctx, current.record.Identity, kindHead)
		if err != nil {
			if isLegacyRepairContextError(err) {
				return report, err
			}
			return report, fmt.Errorf("%w: head=%s: final head re-read failed", ErrLegacyRepairRefused, identityHash)
		}
		latest, err := inspectLegacyRepairHead(ctx, store, latestRecord)
		if err != nil {
			return report, err
		}
		current = latest
		receipt, found, err := loadLegacyRepairReceipt(ctx, store, current.record, toolVersion)
		if err != nil {
			return report, err
		}
		if len(view.duplicates) == 0 {
			// A prior successful apply may have committed its receipt before
			// the caller lost the response. Replay returns that exact receipt
			// even though the current head is already canonical.
			if !found {
				continue
			}
		} else if !ok || len(current.duplicates) != 0 {
			return report, fmt.Errorf("%w: head=%s: duplicate references remain after apply", ErrLegacyRepairRefused, identityHash)
		}
		if !found {
			return report, fmt.Errorf("%w: head=%s: applied receipt is missing", ErrLegacyRepairRefused, identityHash)
		}
		report.AffectedHeadCount++
		report.DuplicateSequenceCount += len(receipt.Duplicates)
		for _, duplicate := range receipt.Duplicates {
			report.RedundantReferenceCount += len(duplicate.Positions) - 1
		}
		report.Heads = append(report.Heads, LegacyRepairHead{
			HeadIdentityHash:   receipt.HeadIdentityHash,
			BeforeHeadHash:     receipt.BeforeHeadHash,
			AfterHeadHash:      receipt.AfterHeadHash,
			ExpectedGeneration: receipt.ExpectedGeneration,
			AppliedGeneration:  receipt.AppliedGeneration,
			Duplicates:         cloneLegacyRepairDuplicates(receipt.Duplicates),
			Outcome:            receipt.Outcome,
			ReceiptID:          receipt.ReceiptID,
		})
	}
	sort.Slice(report.Heads, func(i, j int) bool { return report.Heads[i].HeadIdentityHash < report.Heads[j].HeadIdentityHash })
	return report, nil
}

func legacyRepairReceiptKind(id identity.Quadruple) string {
	return legacyRepairReceiptPrefix + headIdentityHash(id)
}

func headIdentityHash(id identity.Quadruple) string {
	// The identity is hashed as a length-delimited tuple; raw values never
	// appear in operator output or durable repair receipts.
	h := sha256.New()
	for _, part := range []string{id.TenantID, id.UserID, id.SessionID, id.RunID} {
		var length [8]byte
		length[0] = byte(len(part) >> 56)
		length[1] = byte(len(part) >> 48)
		length[2] = byte(len(part) >> 40)
		length[3] = byte(len(part) >> 32)
		length[4] = byte(len(part) >> 24)
		length[5] = byte(len(part) >> 16)
		length[6] = byte(len(part) >> 8)
		length[7] = byte(len(part))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildLegacyRepairReport(mode LegacyRepairMode, toolVersion string, views []legacyRepairHeadView) LegacyRepairReport {
	report := LegacyRepairReport{ToolVersion: toolVersion, Mode: mode, HeadsScanned: len(views)}
	for _, view := range views {
		if len(view.duplicates) == 0 {
			continue
		}
		report.AffectedHeadCount++
		report.DuplicateSequenceCount += len(view.duplicates)
		for _, duplicate := range view.duplicates {
			report.RedundantReferenceCount += len(duplicate.Positions) - 1
		}
		report.Heads = append(report.Heads, LegacyRepairHead{
			HeadIdentityHash:   headIdentityHash(view.record.Identity),
			BeforeHeadHash:     sha256Hex(view.record.Bytes),
			ExpectedGeneration: string(view.record.ID),
			Duplicates:         cloneLegacyRepairDuplicates(view.duplicates),
			Outcome:            "repairable",
		})
	}
	if mode == LegacyRepairVerify && report.AffectedHeadCount == 0 {
		report.Heads = nil
	}
	return report
}

func cloneLegacyRepairDuplicates(in []LegacyRepairDuplicate) []LegacyRepairDuplicate {
	out := make([]LegacyRepairDuplicate, len(in))
	for i, duplicate := range in {
		out[i] = duplicate
		out[i].Positions = append([]int(nil), duplicate.Positions...)
	}
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}
