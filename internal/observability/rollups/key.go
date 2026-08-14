package rollups

import (
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Key identifies one rollup row: a fixed UTC bucket start (on the minute
// grid — the storage granularity) plus the authoritative dimension values.
// Model is the empty string when the source event carried no authoritative
// value for that dimension — the row is then the un-attributed aggregate for
// the triple it shares. A Key is comparable, so it can be a map key directly.
type Key struct {
	// BucketStart is the start instant of the fixed UTC bucket the row
	// belongs to (UTC, on the BucketMinute grid).
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
	default:
		return ""
	}
}

// SessionTriple is the comparable, collision-free key for a session's
// isolation triple. It is the ONLY sanctioned way to key per-session state
// (the erasure fence): identity validation permits NUL characters inside
// ids, so NUL-joining the three strings into one key would alias distinct
// triples (e.g. tenant "a\x00b" vs tenant "a", user "b"). A struct of the
// three string fields is comparable and never aliases.
type SessionTriple struct {
	TenantID  string
	UserID    string
	SessionID string
}

// TripleOf builds the SessionTriple of an identity.
func TripleOf(id identity.Identity) SessionTriple {
	return SessionTriple{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
}

// Matches reports whether the triple equals the row key's session triple.
func (t SessionTriple) Matches(k Key) bool {
	return t.TenantID == k.TenantID && t.UserID == k.UserID && t.SessionID == k.SessionID
}

// Delta is one row update derived from a single canonical event: the bucket
// row to touch and the exact integer measures to accumulate.
type Delta struct {
	Key Key
	Add MeasureSet
}
