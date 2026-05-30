package memory

// Wire shapes shared by every `MemoryStore` driver. Each driver
// marshals to / unmarshals from these structs so `Snapshot` bytes are
// byte-stable across drivers (a snapshot taken from the InMem driver
// MUST round-trip through `Restore` on the SQLite or Postgres driver
// and vice-versa, per Phase 25's acceptance criteria).
//
// The shape is intentionally minimal at Phase 23/25: Strategy + a
// reserved `Turns` slot. Phase 24 will fill `Turns` for the
// `truncation` / `rolling_summary` strategies and (optionally) extend
// the envelope with a rolling-summary text field; that extension is
// additive and must remain backward-compatible (older drivers MUST
// keep round-tripping older snapshots).

// KindMemoryState is the canonical record-kind string Harbor uses to
// route memory state in the persistence layer. Since Phase 25a (D-174)
// every driver — InMem, SQLite, Postgres — persists this kind through
// the injected `state.StateStore` via the shared strategy executor; the
// SQL drivers no longer write a private `memory_state` table on the
// strategy path. The constant is exported so cross-driver tests and
// operators can reference it.
const KindMemoryState = "memory.state"

// Record is the JSON envelope every driver persists as the opaque
// `Snapshot.Bytes` payload. The shape is exported so tests + later
// driver implementations share a single source of truth (D-034 — wire
// envelope centralised in the `memory` package for cross-driver
// byte-stable Snapshot/Restore).
//
// Phase 23 only writes empty records (Strategy=none has no
// mutations). Phase 24 will populate `Turns` for the truncation +
// rolling_summary strategies. The struct's JSON tags pin the wire
// format and MUST NOT change after Phase 23 merged — additive fields
// are fine (later strategies will append fields), but renaming an
// existing field would break cross-driver round-trip.
type Record struct {
	Strategy Strategy           `json:"strategy"`
	Turns    []ConversationTurn `json:"turns,omitempty"`
}
