package durable

// Streaming decode of a legacy (v1) head for migration. A 100k-row legacy head
// is a ~20 MB JSON document; decoding it into headRecord materializes every
// eventMetadataRecord (with its own identity strings), and authenticating it
// re-serializes the whole projection for the checksum. On a small runtime
// that is 100–200 MB of transient heap. The stream below walks the document
// once, authenticates the metadata checksum incrementally (hashing exactly
// the bytes metadataIntegrityChecksum would marshal), and keeps only the
// compact indexRow per event with interned type and run-id strings.
//
// Anything outside the shape encodeHead produces (field order, unknown,
// case-variant or duplicate keys, trailing data, missing fields, an empty
// head) and any row that fails a check returns errLegacyStreamUnsupported:
// the caller falls back to the ordinary decode path, which either backfills an
// unauthenticated head from bodies or fails loudly, so correctness never
// depends on the fast path.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

var errLegacyStreamUnsupported = errors.New("durable: legacy head shape not supported by the streaming decoder")

// legacyInternMax bounds the string-interning tables.
const legacyInternMax = 4096

// streamLegacyRows authenticates a metadata-ready legacy head and returns its
// rows. When prefix is non-nil, the head's first len(prefix) rows must equal
// prefix and are not re-allocated: the result reuses prefix's backing array
// (appending the newer rows), so a flip retry costs only the new rows.
// spare is extra capacity reserved for later appends.
func streamLegacyRows(raw []byte, id identity.Quadruple, prefix []indexRow, spare int) ([]indexRow, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	expectDelim := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != want {
			return errLegacyStreamUnsupported
		}
		return nil
	}
	if err := expectDelim('{'); err != nil {
		return nil, unsupportedOr(err)
	}
	var seqs []uint64
	var rows []indexRow
	hasher := sha256.New()
	types := map[string]events.EventType{}
	runs := map[string]string{}
	var (
		sawSeqs, sawMeta bool
		ready            bool
		validated        int
		checksum         string
	)
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, unsupportedOr(err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errLegacyStreamUnsupported
		}
		// Only the exact canonical keys encodeHead writes, each at most once.
		// encoding/json matches keys case-insensitively and lets a later
		// duplicate win, so anything else could make this stream disagree
		// with the full decoder: decline and let the full path decide.
		if seen[key] {
			return nil, errLegacyStreamUnsupported
		}
		seen[key] = true
		switch key {
		case "sequences":
			if sawSeqs || sawMeta {
				return nil, errLegacyStreamUnsupported
			}
			sawSeqs = true
			if err := expectDelim('['); err != nil {
				return nil, unsupportedOr(err)
			}
			for dec.More() {
				var seq uint64
				if err := dec.Decode(&seq); err != nil {
					return nil, unsupportedOr(err)
				}
				seqs = append(seqs, seq)
			}
			if err := expectDelim(']'); err != nil {
				return nil, unsupportedOr(err)
			}
			if len(seqs) == 0 {
				return nil, errLegacyStreamUnsupported
			}
			if prefix != nil && len(seqs) < len(prefix) {
				return nil, declinef("legacy head shrank below the pre-written prefix")
			}
			// The checksum input is {"sequences":[...],"metadata":[...]}.
			_, _ = hasher.Write([]byte(`{"sequences":[`))
			for i, seq := range seqs {
				if i > 0 {
					_, _ = hasher.Write([]byte{','})
				}
				_, _ = hasher.Write(strconv.AppendUint(nil, seq, 10))
			}
			_, _ = hasher.Write([]byte(`],"metadata":[`))
			if prefix != nil {
				rows = prefix[:len(prefix):max(cap(prefix), len(prefix))]
				if cap(prefix)-len(prefix) < len(seqs)-len(prefix) {
					grown := make([]indexRow, len(prefix), len(seqs)+spare)
					copy(grown, prefix)
					rows = grown
				}
			} else {
				rows = make([]indexRow, 0, len(seqs)+spare)
			}
		case "metadata":
			if !sawSeqs || sawMeta {
				return nil, errLegacyStreamUnsupported
			}
			sawMeta = true
			if err := expectDelim('['); err != nil {
				return nil, unsupportedOr(err)
			}
			i := 0
			for dec.More() {
				if i >= len(seqs) {
					return nil, declinef("legacy metadata has more rows than sequences")
				}
				var m eventMetadataRecord
				if err := dec.Decode(&m); err != nil {
					return nil, unsupportedOr(err)
				}
				encoded, err := json.Marshal(m)
				if err != nil {
					return nil, err
				}
				if i > 0 {
					_, _ = hasher.Write([]byte{','})
				}
				_, _ = hasher.Write(encoded)
				if m.Sequence != seqs[i] {
					return nil, declinef("legacy metadata row %d is out of sequence order", i)
				}
				if m.TenantID != id.TenantID || m.UserID != id.UserID || m.SessionID != id.SessionID {
					return nil, declinef("legacy metadata row %d has a foreign identity", i)
				}
				r := rowFromMetadataRecord(m)
				r.Type = internType(types, r.Type)
				r.RunID = internString(runs, r.RunID)
				if err := validateIndexRow(r); err != nil {
					return nil, declinef("legacy metadata row %d: %v", i, err)
				}
				if i < len(prefix) {
					if r != prefix[i] {
						return nil, declinef("legacy head prefix changed at row %d", i)
					}
				} else {
					if i > 0 && r.Seq <= lastSeq(rows, prefix, i) {
						return nil, declinef("legacy head is not strictly increasing at row %d", i)
					}
					rows = append(rows, r)
				}
				i++
			}
			if err := expectDelim(']'); err != nil {
				return nil, unsupportedOr(err)
			}
			if i != len(seqs) {
				return nil, declinef("legacy metadata has %d rows for %d sequences", i, len(seqs))
			}
			_, _ = hasher.Write([]byte(`]}`))
		case "metadata_ready":
			if err := dec.Decode(&ready); err != nil {
				return nil, unsupportedOr(err)
			}
		case "metadata_validated_count":
			if err := dec.Decode(&validated); err != nil {
				return nil, unsupportedOr(err)
			}
		case "metadata_integrity_checksum":
			if err := dec.Decode(&checksum); err != nil {
				return nil, unsupportedOr(err)
			}
		default:
			return nil, errLegacyStreamUnsupported
		}
	}
	// The document must end exactly here: no trailing data or second value.
	if err := expectDelim('}'); err != nil {
		return nil, unsupportedOr(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errLegacyStreamUnsupported
	}
	if !sawSeqs || !sawMeta || !ready || validated != len(seqs) {
		// Not an authenticated, metadata-ready head: the canonical backfill
		// path (which validates bodies) handles it.
		return nil, errLegacyStreamUnsupported
	}
	if hex.EncodeToString(hasher.Sum(nil)) != checksum {
		// headMetadataReady would reject it too; the backfill rebuilds rows
		// from bodies.
		return nil, errLegacyStreamUnsupported
	}
	return rows, nil
}

func lastSeq(rows, prefix []indexRow, i int) uint64 {
	if len(rows) > 0 {
		return rows[len(rows)-1].Seq
	}
	if i > 0 && i <= len(prefix) {
		return prefix[i-1].Seq
	}
	return 0
}

func unsupportedOr(err error) error {
	if errors.Is(err, errLegacyStreamUnsupported) {
		return err
	}
	return fmt.Errorf("%w: %w", errLegacyStreamUnsupported, err)
}

func internType(table map[string]events.EventType, t events.EventType) events.EventType {
	if v, ok := table[string(t)]; ok {
		return v
	}
	if len(table) < legacyInternMax {
		table[string(t)] = t
	}
	return t
}

func internString(table map[string]string, s string) string {
	if s == "" {
		return s
	}
	if v, ok := table[s]; ok {
		return v
	}
	if len(table) < legacyInternMax {
		table[s] = s
	}
	return s
}

// declinef reports a reason the stream declines the head (unsupported), so
// the caller re-decides on the full decode path.
func declinef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errLegacyStreamUnsupported, fmt.Sprintf(format, args...))
}
