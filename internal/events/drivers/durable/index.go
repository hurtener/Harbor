package durable

// This file owns the v2 chunked durable event index (D-492). A session's
// routing index is split into one small mutable header (the existing
// `events.durable.head` slot, magic-prefixed so binaries below v1.37.0 fail
// closed on it) and immutable sealed chunks of exactly indexChunkRows rows.
// The header carries the unsealed tail plus every bound a reader needs, so a
// publish reads and writes O(chunk) bytes instead of the whole history.
//
// Integrity model. Sealed chunks form a SHA-256 chain: each chunk's digest
// covers its predecessor's digest plus every routing field (index, first and
// last sequence, min/max OccurredAt, rows). The header authenticates the
// newest chunk through sealed_digest and the oldest retained chunk's
// predecessor through base_digest, and header_digest covers every header
// field including the tail rows. Readers recompute every derived field from
// the rows and compare it with the stored value, so a routing field is both
// digest-bound and derived. Any mismatch is ErrIndexIntegrity: the read or
// publish fails loudly and nothing is re-derived on read (repair is an
// explicit operator restore).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	// kindChunkPrefix is the StateStore Kind prefix of a sealed v2 index
	// chunk; the zero-padded chunk index is appended. The prefix is distinct
	// from kindHead, so the maintenance head scan never loads chunks.
	kindChunkPrefix = "events.durable.chunk/"
	// kindRetentionSummary is the cumulative audit-retention summary record
	// stored beside the header under the same session key (D-493).
	kindRetentionSummary = "events.durable.retention_summary"
	// indexMagic prefixes every v2 header and chunk. JSON decoders in older
	// binaries reject it, which is the fail-closed rollback boundary.
	indexMagic = "HXIDX2\n"
	// indexFormat is the persisted index format version.
	indexFormat = 2
	// indexChunkRows is the fixed number of rows in every sealed chunk.
	indexChunkRows = 256
	// inlineMigrationMaxRows is the largest legacy head the publish path
	// converts inside its own transaction. Larger heads keep appending in the
	// legacy format until the background migrator flips them.
	inlineMigrationMaxRows = 1024
	// indexHeaderDomain / indexChunkDomain separate the two digest inputs.
	indexHeaderDomain = "harbor.events.index.header.v2\n"
	indexChunkDomain  = "harbor.events.index.chunk.v2\n"
)

// genesisDigest is the predecessor digest of chunk 0.
var genesisDigest = strings.Repeat("0", sha256.Size*2)

// ErrIndexIntegrity reports that a v2 event index header or chunk failed
// digest, chain, or derived-field verification. The affected read or publish
// fails for that session only; other sessions are unaffected. There is no
// automatic re-derivation: recovery is an explicit operator restore.
var ErrIndexIntegrity = errors.New("durable: event index integrity failure")

// indexRow is the compact routing projection of one persisted event. The
// tenant/user/session identity is implied by the storage slot. Field order is
// fixed: encoding/json emits struct fields in declaration order, which makes
// the row encoding canonical across processes and drivers.
type indexRow struct {
	Seq         uint64           `json:"seq"`
	Type        events.EventType `json:"type"`
	RunID       string           `json:"run_id,omitempty"`
	OccurredAt  int64            `json:"occurred_at"`
	Internal    bool             `json:"internal,omitempty"`
	CostSummary bool             `json:"cost_summary,omitempty"`
	CostDollars float64          `json:"cost_dollars,omitempty"`
	TotalTokens int64            `json:"total_tokens,omitempty"`
}

// indexTrimmed records the audit-retention cut. Every sequence at or below
// ThroughSeq is gone from the index. CleanupPending marks that trimmed chunk
// or entry records may still be present in storage; PendingEntries lists the
// entry sequences of a trimmed tail, which no chunk record names.
type indexTrimmed struct {
	Count          uint64   `json:"count"`
	ThroughSeq     uint64   `json:"through_seq"`
	CleanupPending bool     `json:"cleanup_pending,omitempty"`
	PendingEntries []uint64 `json:"pending_entries,omitempty"`
}

// indexHeader is the mutable per-session v2 index header. Sealed chunks are
// [FirstChunk, NextChunk). HeaderDigest authenticates every other field.
type indexHeader struct {
	Format           int          `json:"format"`
	ChunkRows        int          `json:"chunk_rows"`
	FirstChunk       uint64       `json:"first_chunk"`
	NextChunk        uint64       `json:"next_chunk"`
	BaseDigest       string       `json:"base_digest"`
	SealedDigest     string       `json:"sealed_digest"`
	Tail             []indexRow   `json:"tail"`
	RetainedCount    uint64       `json:"retained_count"`
	FirstSeq         uint64       `json:"first_seq"`
	LastSeq          uint64       `json:"last_seq"`
	OldestOccurredAt int64        `json:"oldest_occurred_at"`
	Trimmed          indexTrimmed `json:"trimmed"`
	HeaderDigest     string       `json:"header_digest,omitempty"`
}

// indexChunk is one immutable sealed chunk of exactly indexChunkRows rows.
type indexChunk struct {
	Format        int        `json:"format"`
	Index         uint64     `json:"index"`
	Prev          string     `json:"prev"`
	FirstSeq      uint64     `json:"first_seq"`
	LastSeq       uint64     `json:"last_seq"`
	MinOccurredAt int64      `json:"min_occurred_at"`
	MaxOccurredAt int64      `json:"max_occurred_at"`
	Rows          []indexRow `json:"rows"`
	Digest        string     `json:"digest"`
}

// chunkDigestBody is the canonical digest input of a chunk. Prev is hashed
// separately, ahead of the body, so the chain link is explicit.
type chunkDigestBody struct {
	Format        int        `json:"format"`
	Index         uint64     `json:"index"`
	FirstSeq      uint64     `json:"first_seq"`
	LastSeq       uint64     `json:"last_seq"`
	MinOccurredAt int64      `json:"min_occurred_at"`
	MaxOccurredAt int64      `json:"max_occurred_at"`
	Rows          []indexRow `json:"rows"`
}

func newIndexHeader() indexHeader {
	return indexHeader{
		Format:       indexFormat,
		ChunkRows:    indexChunkRows,
		BaseDigest:   genesisDigest,
		SealedDigest: genesisDigest,
	}
}

func chunkKind(index uint64) string {
	return fmt.Sprintf("%s%020d", kindChunkPrefix, index)
}

// parseChunkKind returns the chunk index named by kind.
func parseChunkKind(kind string) (uint64, bool) {
	if !strings.HasPrefix(kind, kindChunkPrefix) {
		return 0, false
	}
	token := kind[len(kindChunkPrefix):]
	if len(token) != 20 {
		return 0, false
	}
	var index uint64
	for _, c := range token {
		if c < '0' || c > '9' {
			return 0, false
		}
		index = index*10 + uint64(c-'0')
	}
	return index, chunkKind(index) == kind
}

// isIndexV2 reports whether stored head bytes are a v2 header.
func isIndexV2(b []byte) bool {
	return bytes.HasPrefix(b, []byte(indexMagic))
}

func rowFromMetadataRecord(m eventMetadataRecord) indexRow {
	return indexRow{
		Seq:         m.Sequence,
		Type:        m.Type,
		RunID:       m.RunID,
		OccurredAt:  m.OccurredAt,
		Internal:    m.Internal,
		CostSummary: m.CostSummary,
		CostDollars: m.CostDollars,
		TotalTokens: m.TotalTokens,
	}
}

func rowFromEvent(ev events.Event) (indexRow, error) {
	m, err := metadataRecordFromEvent(ev)
	if err != nil {
		return indexRow{}, err
	}
	return rowFromMetadataRecord(m), nil
}

// metadataRecord re-expands a row with the identity implied by its slot.
func (r indexRow) metadataRecord(id identity.Quadruple) eventMetadataRecord {
	return eventMetadataRecord{
		Type:        r.Type,
		TenantID:    id.TenantID,
		UserID:      id.UserID,
		SessionID:   id.SessionID,
		RunID:       r.RunID,
		OccurredAt:  r.OccurredAt,
		Sequence:    r.Seq,
		Internal:    r.Internal,
		CostSummary: r.CostSummary,
		CostDollars: r.CostDollars,
		TotalTokens: r.TotalTokens,
	}
}

func (r indexRow) metadata(id identity.Quadruple) events.EventMetadata {
	return r.metadataRecord(id).metadata()
}

func validateIndexRow(r indexRow) error {
	if r.Seq == 0 {
		return fmt.Errorf("row has zero sequence")
	}
	if r.Type == "" || !events.IsValidEventType(r.Type) {
		return fmt.Errorf("row seq=%d has unknown event type %q", r.Seq, r.Type)
	}
	if r.Internal != events.IsBusInternalNotice(r.Type) {
		return fmt.Errorf("row seq=%d internal marker disagrees with type %q", r.Seq, r.Type)
	}
	if math.IsNaN(r.CostDollars) || math.IsInf(r.CostDollars, 0) {
		return fmt.Errorf("row seq=%d has non-finite cost", r.Seq)
	}
	return nil
}

// validateRowRun checks rows are valid and strictly increasing by sequence,
// strictly above floor.
func validateRowRun(rows []indexRow, floor uint64) error {
	previous := floor
	for i, r := range rows {
		if err := validateIndexRow(r); err != nil {
			return err
		}
		if r.Seq <= previous && (i > 0 || floor > 0) {
			return fmt.Errorf("row seq=%d is not strictly above %d", r.Seq, previous)
		}
		previous = r.Seq
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func integrityErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrIndexIntegrity, fmt.Sprintf(format, args...))
}

// computeChunkDigest returns SHA256(domain || prev || canonical body).
func computeChunkDigest(c indexChunk) (string, error) {
	body, err := json.Marshal(chunkDigestBody{
		Format:        c.Format,
		Index:         c.Index,
		FirstSeq:      c.FirstSeq,
		LastSeq:       c.LastSeq,
		MinOccurredAt: c.MinOccurredAt,
		MaxOccurredAt: c.MaxOccurredAt,
		Rows:          c.Rows,
	})
	if err != nil {
		return "", fmt.Errorf("encode chunk digest input: %w", err)
	}
	h := sha256.New()
	_, _ = h.Write([]byte(indexChunkDomain))
	_, _ = h.Write([]byte(c.Prev))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sealChunk builds the deterministic sealed chunk for rows at index with the
// given predecessor digest. The same rows, index and predecessor always yield
// byte-identical encoded chunks, which makes migration pre-writes idempotent.
func sealChunk(index uint64, prev string, rows []indexRow) (indexChunk, error) {
	if len(rows) != indexChunkRows {
		return indexChunk{}, fmt.Errorf("seal chunk %d: %d rows, want %d", index, len(rows), indexChunkRows)
	}
	c := indexChunk{
		Format:   indexFormat,
		Index:    index,
		Prev:     prev,
		FirstSeq: rows[0].Seq,
		LastSeq:  rows[len(rows)-1].Seq,
		Rows:     append([]indexRow(nil), rows...),
	}
	c.MinOccurredAt, c.MaxOccurredAt = rows[0].OccurredAt, rows[0].OccurredAt
	for _, r := range rows[1:] {
		if r.OccurredAt < c.MinOccurredAt {
			c.MinOccurredAt = r.OccurredAt
		}
		if r.OccurredAt > c.MaxOccurredAt {
			c.MaxOccurredAt = r.OccurredAt
		}
	}
	digest, err := computeChunkDigest(c)
	if err != nil {
		return indexChunk{}, err
	}
	c.Digest = digest
	return c, nil
}

func encodeChunk(c indexChunk) ([]byte, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("durable: marshal index chunk %d: %w", c.Index, err)
	}
	return append([]byte(indexMagic), body...), nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// decodeChunk decodes and fully verifies one sealed chunk read from the slot
// for wantIndex: magic, format, index, own digest, row validity and order,
// and every derived routing field. It does not verify the chain link; the
// caller compares Digest with the expected link.
func decodeChunk(b []byte, wantIndex uint64) (indexChunk, error) {
	if !isIndexV2(b) {
		return indexChunk{}, integrityErrorf("chunk %d has no v2 magic prefix", wantIndex)
	}
	var c indexChunk
	if err := strictUnmarshal(b[len(indexMagic):], &c); err != nil {
		return indexChunk{}, integrityErrorf("chunk %d is malformed: %v", wantIndex, err)
	}
	if c.Format != indexFormat {
		return indexChunk{}, integrityErrorf("chunk %d has format %d", wantIndex, c.Format)
	}
	if c.Index != wantIndex {
		return indexChunk{}, integrityErrorf("chunk slot %d holds index %d", wantIndex, c.Index)
	}
	if !validDigest(c.Prev) || !validDigest(c.Digest) {
		return indexChunk{}, integrityErrorf("chunk %d has malformed digests", wantIndex)
	}
	if len(c.Rows) != indexChunkRows {
		return indexChunk{}, integrityErrorf("chunk %d has %d rows, want %d", wantIndex, len(c.Rows), indexChunkRows)
	}
	if err := validateRowRun(c.Rows, 0); err != nil {
		return indexChunk{}, integrityErrorf("chunk %d: %v", wantIndex, err)
	}
	// Derived fields: recompute from the rows and compare with the stored
	// values. A routing field is therefore authenticated twice — by the digest
	// and by derivation.
	derived, err := sealChunk(c.Index, c.Prev, c.Rows)
	if err != nil {
		return indexChunk{}, integrityErrorf("chunk %d: %v", wantIndex, err)
	}
	if derived.FirstSeq != c.FirstSeq || derived.LastSeq != c.LastSeq ||
		derived.MinOccurredAt != c.MinOccurredAt || derived.MaxOccurredAt != c.MaxOccurredAt {
		return indexChunk{}, integrityErrorf("chunk %d routing fields disagree with its rows", wantIndex)
	}
	digest, err := computeChunkDigest(c)
	if err != nil {
		return indexChunk{}, integrityErrorf("chunk %d: %v", wantIndex, err)
	}
	if digest != c.Digest {
		return indexChunk{}, integrityErrorf("chunk %d digest mismatch", wantIndex)
	}
	return c, nil
}

func computeHeaderDigest(h indexHeader) (string, error) {
	h.HeaderDigest = ""
	body, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode header digest input: %w", err)
	}
	sum := sha256.Sum256(append([]byte(indexHeaderDomain), body...))
	return hex.EncodeToString(sum[:]), nil
}

// encodeHeader validates h, stamps header_digest, and returns magic + JSON.
// Encoding an invalid header is a programming error and fails loudly.
func encodeHeader(h indexHeader) ([]byte, error) {
	h.HeaderDigest = ""
	if err := validateHeader(h); err != nil {
		return nil, fmt.Errorf("durable: refuse to encode invalid index header: %w", err)
	}
	digest, err := computeHeaderDigest(h)
	if err != nil {
		return nil, err
	}
	h.HeaderDigest = digest
	body, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("durable: marshal index header: %w", err)
	}
	return append([]byte(indexMagic), body...), nil
}

// decodeHeader decodes a v2 header and verifies its digest and every derived
// invariant that is checkable without loading chunks.
func decodeHeader(b []byte) (indexHeader, error) {
	if !isIndexV2(b) {
		return indexHeader{}, integrityErrorf("header has no v2 magic prefix")
	}
	var h indexHeader
	if err := strictUnmarshal(b[len(indexMagic):], &h); err != nil {
		return indexHeader{}, integrityErrorf("header is malformed: %v", err)
	}
	if !validDigest(h.HeaderDigest) {
		return indexHeader{}, integrityErrorf("header digest is malformed")
	}
	digest, err := computeHeaderDigest(h)
	if err != nil {
		return indexHeader{}, integrityErrorf("%v", err)
	}
	if digest != h.HeaderDigest {
		return indexHeader{}, integrityErrorf("header digest mismatch")
	}
	if err := validateHeader(h); err != nil {
		return indexHeader{}, err
	}
	return h, nil
}

// validateHeader checks the header's internal derived invariants.
func validateHeader(h indexHeader) error {
	if h.Format != indexFormat || h.ChunkRows != indexChunkRows {
		return integrityErrorf("header format=%d chunk_rows=%d", h.Format, h.ChunkRows)
	}
	if !validDigest(h.BaseDigest) || !validDigest(h.SealedDigest) {
		return integrityErrorf("header chain digests are malformed")
	}
	if h.FirstChunk > h.NextChunk {
		return integrityErrorf("header first_chunk=%d exceeds next_chunk=%d", h.FirstChunk, h.NextChunk)
	}
	if h.FirstChunk == h.NextChunk && h.BaseDigest != h.SealedDigest {
		return integrityErrorf("header without sealed chunks has diverging chain digests")
	}
	if len(h.Tail) > indexChunkRows {
		return integrityErrorf("header tail has %d rows, above %d", len(h.Tail), indexChunkRows)
	}
	if err := validateRowRun(h.Tail, 0); err != nil {
		return integrityErrorf("header tail: %v", err)
	}
	chunks := h.NextChunk - h.FirstChunk
	if chunks > (math.MaxUint64-uint64(len(h.Tail)))/indexChunkRows {
		return integrityErrorf("header chunk range overflows")
	}
	if want := chunks*indexChunkRows + uint64(len(h.Tail)); h.RetainedCount != want {
		return integrityErrorf("header retained_count=%d, want %d", h.RetainedCount, want)
	}
	if h.Trimmed.Count == 0 && (h.Trimmed.ThroughSeq != 0 || h.Trimmed.CleanupPending || len(h.Trimmed.PendingEntries) != 0) {
		return integrityErrorf("header carries trim bookkeeping without a trim")
	}
	if h.Trimmed.Count > 0 && h.Trimmed.ThroughSeq == 0 {
		return integrityErrorf("header trim has no through sequence")
	}
	if len(h.Trimmed.PendingEntries) > indexChunkRows {
		return integrityErrorf("header lists %d pending entry deletes, above %d", len(h.Trimmed.PendingEntries), indexChunkRows)
	}
	for _, seq := range h.Trimmed.PendingEntries {
		if seq == 0 || seq > h.Trimmed.ThroughSeq {
			return integrityErrorf("header pending entry seq=%d outside the trimmed range", seq)
		}
	}
	if h.LastSeq < h.Trimmed.ThroughSeq {
		return integrityErrorf("header last_seq=%d is below trimmed through_seq=%d", h.LastSeq, h.Trimmed.ThroughSeq)
	}
	if h.RetainedCount == 0 {
		if h.FirstSeq != 0 || h.OldestOccurredAt != 0 || h.LastSeq != h.Trimmed.ThroughSeq {
			return integrityErrorf("empty header bounds first=%d last=%d oldest=%d through=%d", h.FirstSeq, h.LastSeq, h.OldestOccurredAt, h.Trimmed.ThroughSeq)
		}
		return nil
	}
	if h.FirstSeq == 0 || h.FirstSeq > h.LastSeq {
		return integrityErrorf("header bounds first=%d last=%d", h.FirstSeq, h.LastSeq)
	}
	if h.Trimmed.Count > 0 && h.FirstSeq <= h.Trimmed.ThroughSeq {
		return integrityErrorf("header first_seq=%d is not above trimmed through_seq=%d", h.FirstSeq, h.Trimmed.ThroughSeq)
	}
	if len(h.Tail) > 0 {
		if h.LastSeq != h.Tail[len(h.Tail)-1].Seq {
			return integrityErrorf("header last_seq=%d disagrees with tail", h.LastSeq)
		}
		if chunks == 0 && h.FirstSeq != h.Tail[0].Seq {
			return integrityErrorf("header first_seq=%d disagrees with tail", h.FirstSeq)
		}
		for _, r := range h.Tail {
			if h.OldestOccurredAt > r.OccurredAt {
				return integrityErrorf("header oldest_occurred_at is above tail seq=%d", r.Seq)
			}
		}
	}
	return nil
}

// appendRows appends strictly increasing rows to the header and seals every
// full leading chunk while the tail exceeds indexChunkRows rows. It returns
// the newly sealed chunks; the caller writes them expected-absent in the same
// transaction as the header.
func (h *indexHeader) appendRows(rows []indexRow) ([]indexChunk, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	if rows[0].Seq <= h.LastSeq {
		// The authority read that assigned these sequences is older than the
		// header: another writer committed meanwhile. The batch's authority
		// CAS would fail anyway; report a retryable conflict.
		return nil, fmt.Errorf("append seq=%d at or below index last_seq=%d: %w", rows[0].Seq, h.LastSeq, state.ErrConditionFailed)
	}
	if err := validateRowRun(rows, h.LastSeq); err != nil {
		return nil, fmt.Errorf("append to index: %w", err)
	}
	wasEmpty := h.RetainedCount == 0
	tail := make([]indexRow, 0, len(h.Tail)+len(rows))
	tail = append(tail, h.Tail...)
	tail = append(tail, rows...)
	var sealed []indexChunk
	for len(tail) > indexChunkRows {
		c, err := sealChunk(h.NextChunk, h.SealedDigest, tail[:indexChunkRows])
		if err != nil {
			return nil, err
		}
		sealed = append(sealed, c)
		h.SealedDigest = c.Digest
		h.NextChunk++
		tail = append([]indexRow(nil), tail[indexChunkRows:]...)
	}
	h.Tail = tail
	h.RetainedCount += uint64(len(rows))
	h.LastSeq = rows[len(rows)-1].Seq
	if wasEmpty {
		h.FirstSeq = rows[0].Seq
		h.OldestOccurredAt = rows[0].OccurredAt
	}
	for _, r := range rows {
		if r.OccurredAt < h.OldestOccurredAt {
			h.OldestOccurredAt = r.OccurredAt
		}
	}
	return sealed, nil
}

// buildMigratedHeader builds the v2 header for a legacy row set whose full
// leading chunks are the given pre-sealed chunks.
func buildMigratedHeader(rows []indexRow, chunks []indexChunk) (indexHeader, error) {
	h := newIndexHeader()
	if len(rows) == 0 {
		return h, nil
	}
	if len(chunks)*indexChunkRows > len(rows) || len(rows)-len(chunks)*indexChunkRows > indexChunkRows {
		return indexHeader{}, fmt.Errorf("migrated header: %d chunks do not cover %d rows", len(chunks), len(rows))
	}
	prev := genesisDigest
	for i, c := range chunks {
		if c.Index != uint64(i) || c.Prev != prev {
			return indexHeader{}, fmt.Errorf("migrated header: chunk %d is out of chain order", i)
		}
		prev = c.Digest
	}
	h.NextChunk = uint64(len(chunks))
	h.SealedDigest = prev
	h.Tail = append([]indexRow{}, rows[len(chunks)*indexChunkRows:]...)
	h.RetainedCount = uint64(len(rows))
	h.FirstSeq = rows[0].Seq
	h.LastSeq = rows[len(rows)-1].Seq
	h.OldestOccurredAt = rows[0].OccurredAt
	for _, r := range rows {
		if r.OccurredAt < h.OldestOccurredAt {
			h.OldestOccurredAt = r.OccurredAt
		}
	}
	return h, nil
}

// legacyRows converts an authenticated legacy head to sorted index rows. It
// requires strictly increasing sequences, which migration needs.
func legacyRows(head headRecord) ([]indexRow, error) {
	rows := make([]indexRow, len(head.Metadata))
	for i, m := range head.Metadata {
		if i < len(head.Sequences) && head.Sequences[i] != m.Sequence {
			return nil, fmt.Errorf("legacy metadata row %d is out of sequence order", i)
		}
		rows[i] = rowFromMetadataRecord(m)
	}
	if err := validateRowRun(rows, 0); err != nil {
		return nil, fmt.Errorf("legacy head is not strictly increasing: %w", err)
	}
	return rows, nil
}

// sealedChunksFor deterministically seals every full leading chunk of rows.
func sealedChunksFor(rows []indexRow) ([]indexChunk, error) {
	full := len(rows) / indexChunkRows
	out := make([]indexChunk, 0, full)
	prev := genesisDigest
	for i := 0; i < full; i++ {
		c, err := sealChunk(uint64(i), prev, rows[i*indexChunkRows:(i+1)*indexChunkRows])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		prev = c.Digest
	}
	return out, nil
}

func isAuditType(t events.EventType) bool {
	return strings.HasPrefix(string(t), "audit.")
}
