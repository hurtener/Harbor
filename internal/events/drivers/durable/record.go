package durable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// persistedEvent is the on-disk shape the durable driver writes through
// the StateStore. It is JSON-encoded into StateRecord.Bytes.
//
// Payload is stored as a generic JSON object, NOT a typed
// events.EventPayload: StateStore.Bytes is opaque and the concrete
// payload type cannot be reconstructed without a registry the durable
// log deliberately does not own. On replay the payload is rehydrated
// as events.RedactedMap{Data: ...} — the exact same generic
// post-redaction shape the inmem bus already produces for any payload
// that is not SafePayload (see internal/events/drivers/inmem
// wrapRedacted). Replay consumers read fields via RedactedMap.Data;
// persistedEvent records this fidelity boundary.
type persistedEvent struct {
	Type       events.EventType  `json:"type"`
	TenantID   string            `json:"tenant_id"`
	UserID     string            `json:"user_id"`
	SessionID  string            `json:"session_id"`
	RunID      string            `json:"run_id,omitempty"`
	OccurredAt int64             `json:"occurred_at"` // unix nanoseconds
	Sequence   uint64            `json:"sequence"`
	Payload    map[string]any    `json:"payload"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// encodeEvent serialises ev into opaque StateStore bytes. It fails
// loudly (CLAUDE.md §5 "fail loudly") rather than dropping context
// when the payload cannot be marshalled — a silently-dropped event
// would foreclose the gap-free guarantee exists to provide.
func encodeEvent(ev events.Event) ([]byte, error) {
	payload, err := marshalPayload(ev.Payload)
	if err != nil {
		return nil, fmt.Errorf("durable: encode event seq=%d type=%q: %w",
			ev.Sequence, ev.Type, err)
	}
	pe := persistedEvent{
		Type:       ev.Type,
		TenantID:   ev.Identity.TenantID,
		UserID:     ev.Identity.UserID,
		SessionID:  ev.Identity.SessionID,
		RunID:      ev.Identity.RunID,
		OccurredAt: ev.OccurredAt.UnixNano(),
		Sequence:   ev.Sequence,
		Payload:    payload,
		Extra:      ev.Extra,
	}
	b, err := json.Marshal(pe)
	if err != nil {
		return nil, fmt.Errorf("durable: marshal persisted event seq=%d: %w",
			ev.Sequence, err)
	}
	return b, nil
}

// marshalPayload normalises any events.EventPayload to a
// map[string]any so the persisted shape is uniform. A nil payload is
// rejected — ValidateEvent already guarantees non-nil, so this is a
// defence-in-depth assertion, not a silent-degradation path.
func marshalPayload(p events.EventPayload) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil payload")
	}
	// RedactedMap is already a generic map — pass its Data through
	// directly so a re-encoded replayed event is byte-stable.
	if rm, ok := p.(events.RedactedMap); ok {
		return rm.Data, nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// The payload marshalled to something that is not a JSON
		// object (e.g. a bare scalar). Wrap it so the persisted shape
		// stays a map and no information is lost.
		var v any
		if err2 := json.Unmarshal(raw, &v); err2 != nil {
			return nil, fmt.Errorf("payload is not JSON-representable: %w", err)
		}
		return map[string]any{"value": v}, nil
	}
	return m, nil
}

// headRecord is the per-session index the durable driver maintains: the
// ordered list of bus-sequences plus their payload-free routing metadata.
// StateStore has no list/scan method, so this record IS the index —
// Replay reads it to learn which entry records exist for a session.
type headRecord struct {
	Sequences                 []uint64              `json:"sequences"`
	Metadata                  []eventMetadataRecord `json:"metadata,omitempty"`
	MetadataReady             bool                  `json:"metadata_ready,omitempty"`
	MetadataValidatedCount    int                   `json:"metadata_validated_count,omitempty"`
	MetadataIntegrityChecksum string                `json:"metadata_integrity_checksum,omitempty"`
}

// headMetadataDigest is the canonical input to the persisted integrity
// marker. The readiness/checkpoint fields themselves are deliberately not
// included: a marker authenticates the ordered sequence/metadata projection,
// not its bookkeeping.
type headMetadataDigest struct {
	Sequences []uint64              `json:"sequences"`
	Metadata  []eventMetadataRecord `json:"metadata"`
}

// metadataIntegrityChecksum returns a stable lowercase SHA-256 for the
// ordered head projection. JSON is used only for the private, fixed-shape
// record above; encoding/json emits struct fields in declaration order, so
// the digest is stable across restarts and drivers.
func metadataIntegrityChecksum(sequences []uint64, metadata []eventMetadataRecord) string {
	digest, _ := json.Marshal(headMetadataDigest{
		Sequences: sequences,
		Metadata:  metadata,
	})
	sum := sha256.Sum256(digest)
	return hex.EncodeToString(sum[:])
}

// eventMetadataRecord is the durable wire shape of events.EventMetadata.
// Keeping the shape private prevents the StateStore's opaque bytes from
// becoming a second public event contract while allowing old v1.29 heads
// (which only contain Sequences) to be upgraded idempotently.
type eventMetadataRecord struct {
	Type        events.EventType `json:"type"`
	TenantID    string           `json:"tenant_id"`
	UserID      string           `json:"user_id"`
	SessionID   string           `json:"session_id"`
	RunID       string           `json:"run_id,omitempty"`
	OccurredAt  int64            `json:"occurred_at"`
	Sequence    uint64           `json:"sequence"`
	Internal    bool             `json:"internal,omitempty"`
	CostSummary bool             `json:"cost_summary,omitempty"`
	CostDollars float64          `json:"cost_dollars,omitempty"`
	TotalTokens int64            `json:"total_tokens,omitempty"`
}

func metadataRecordFromEvent(ev events.Event) (eventMetadataRecord, error) {
	m, err := events.NewEventMetadata(ev)
	if err != nil {
		return eventMetadataRecord{}, err
	}
	return eventMetadataRecord{
		Type:        m.Type,
		TenantID:    m.Identity.TenantID,
		UserID:      m.Identity.UserID,
		SessionID:   m.Identity.SessionID,
		RunID:       m.Identity.RunID,
		OccurredAt:  m.OccurredAt.UnixNano(),
		Sequence:    m.Sequence,
		Internal:    m.Internal,
		CostSummary: m.CostSummary,
		CostDollars: m.CostDollars,
		TotalTokens: m.TotalTokens,
	}, nil
}

func (m eventMetadataRecord) metadata() events.EventMetadata {
	return events.EventMetadata{
		Type: m.Type,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: m.TenantID, UserID: m.UserID, SessionID: m.SessionID,
		}, RunID: m.RunID},
		OccurredAt:  unixNanoToTime(m.OccurredAt),
		Sequence:    m.Sequence,
		Internal:    m.Internal,
		CostSummary: m.CostSummary,
		CostDollars: m.CostDollars,
		TotalTokens: m.TotalTokens,
	}
}

// encodeHead serialises a headRecord into opaque StateStore bytes.
func encodeHead(h headRecord) ([]byte, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("durable: marshal head record: %w", err)
	}
	return b, nil
}

// decodeHead reverses encodeHead. An empty byte slice decodes to an
// empty head (no sequences yet).
func decodeHead(b []byte) (headRecord, error) {
	if len(b) == 0 {
		return headRecord{}, nil
	}
	var h headRecord
	if err := json.Unmarshal(b, &h); err != nil {
		return headRecord{}, fmt.Errorf("durable: unmarshal head record: %w", err)
	}
	return h, nil
}

// decodeEvent reverses encodeEvent. The rehydrated Event's Payload is
// always an events.RedactedMap — see persistedEvent's doc comment.
func decodeEvent(b []byte) (events.Event, error) {
	var pe persistedEvent
	if err := json.Unmarshal(b, &pe); err != nil {
		return events.Event{}, fmt.Errorf("durable: unmarshal persisted event: %w", err)
	}
	data := pe.Payload
	if data == nil {
		data = map[string]any{}
	}
	return events.Event{
		Type: pe.Type,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  pe.TenantID,
				UserID:    pe.UserID,
				SessionID: pe.SessionID,
			},
			RunID: pe.RunID,
		},
		OccurredAt: unixNanoToTime(pe.OccurredAt),
		Sequence:   pe.Sequence,
		Payload:    events.RedactedMap{Data: data},
		Extra:      pe.Extra,
	}, nil
}
