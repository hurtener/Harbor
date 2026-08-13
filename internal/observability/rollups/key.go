package rollups

import "time"

// Key identifies one rollup row: a fixed UTC bucket start plus the
// authoritative dimension values. Model and Agent are the empty string when
// the source event carried no authoritative value for that dimension — the
// row is then the un-attributed aggregate for the triple it shares. A Key is
// comparable, so it can be a map key directly.
//
// The bucket start is always at StoreGranularity — the finest closed bucket
// size (BucketHour); queries coarsen at read time (see query.go).
type Key struct {
	// BucketStart is the start instant of the fixed UTC bucket the row
	// belongs to (UTC, on the BucketHour grid).
	BucketStart time.Time
	// TenantID is the event's tenant_id (authoritative — the isolation
	// principal).
	TenantID string
	// UserID is the event's user_id.
	UserID string
	// SessionID is the event's session_id.
	SessionID string
	// Model is the source payload's model; empty when the event carried
	// no authoritative model (e.g. task outcome events).
	Model string
	// AgentID is the source payload's authoritative agent id; empty by
	// construction for every V1 canonical payload (see the package doc).
	AgentID string
}

// DimensionValue returns the row's value for a closed dimension. Unknown
// dimensions return "" (ValidateDimensions is the fail-loud entry point for
// untrusted input).
func (k Key) DimensionValue(d Dimension) string {
	switch d {
	case DimensionTenant:
		return k.TenantID
	case DimensionUser:
		return k.UserID
	case DimensionSession:
		return k.SessionID
	case DimensionModel:
		return k.Model
	case DimensionAgent:
		return k.AgentID
	default:
		return ""
	}
}

// Delta is one row update derived from a single canonical event: the bucket
// row to touch and the exact measures to accumulate.
type Delta struct {
	Key Key
	Add MeasureSet
}
