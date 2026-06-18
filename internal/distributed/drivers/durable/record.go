package durable

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/distributed"
)

// entryKindPrefix is the StateStore Kind prefix under which the durable
// bus persists one record per published envelope. Each entry is keyed
// by a fresh time-ordered ULID, so the prefix scan (ListKind) returns
// every persisted envelope in roughly publication order across all
// instances sharing the store. The prefix is disjoint from the durable
// event log (events.durable.*) and the durable task driver
// (task.durable.*), so the three coexist in one shared StateStore.
const entryKindPrefix = "distributed.bus.entry/"

// persistedEnvelope is the on-disk shape for one bus envelope. The
// BusEnvelope round-trips through JSON (its Payload is json.RawMessage;
// Identity / TaskID / EventID / Edge / Source / Target are scalars).
// Meta is best-effort (drivers must not depend on it for correctness),
// so a JSON type-shift inside Meta is acceptable.
type persistedEnvelope struct {
	Envelope distributed.BusEnvelope `json:"envelope"`
}

// encodeEnvelope serialises env into opaque StateStore bytes. It fails
// loudly (CLAUDE.md §5) rather than dropping the envelope when it
// cannot be marshalled (e.g. a non-JSON Payload or a non-serializable
// Meta value) — a silently-dropped envelope would foreclose the
// at-least-once guarantee the durable bus exists to provide.
func encodeEnvelope(env distributed.BusEnvelope) ([]byte, error) {
	b, err := json.Marshal(persistedEnvelope{Envelope: env})
	if err != nil {
		return nil, fmt.Errorf("distributed/durable: marshal envelope (edge=%q event_id=%q): %w",
			env.Edge, env.EventID, err)
	}
	return b, nil
}

// decodeEnvelope reverses encodeEnvelope.
func decodeEnvelope(b []byte) (distributed.BusEnvelope, error) {
	var pe persistedEnvelope
	if err := json.Unmarshal(b, &pe); err != nil {
		return distributed.BusEnvelope{}, fmt.Errorf("distributed/durable: unmarshal envelope: %w", err)
	}
	return pe.Envelope, nil
}
