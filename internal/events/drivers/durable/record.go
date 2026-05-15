package durable

import (
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
// wrapRedacted). Replay consumers read fields via RedactedMap.Data.
// D-074 records this fidelity boundary.
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
// would foreclose the gap-free guarantee Phase 57 exists to provide.
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

// headRecord is the per-session index the durable driver maintains:
// the ordered list of bus-sequences persisted for that session.
// StateStore has no list/scan method, so this record IS the index —
// Replay reads it to learn which entry records exist for a session.
type headRecord struct {
	Sequences []uint64 `json:"sequences"`
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
