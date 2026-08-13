// Package turns owns Harbor's runtime-side conversation-turn
// projection: the durable, bounded, tail-paged read model that backs
// the future `sessions.turns.list` / `sessions.turns.get` Protocol
// surface.
//
// # What a turn is
//
// A turn is one ROOT FOREGROUND run inside a session: the unit a
// human sees as "one exchange" in the conversation. Child / spawned
// tasks are NOT turns — they are activity WITHIN a turn. The runtime
// wiring that feeds this projection selects root foreground runs
// (kind `foreground`, no parent task) before calling Append; this
// package is the projection core and does not re-derive that
// selection (it deliberately has no dependency on the tasks package —
// the turn id IS the run's task id, carried as an opaque string).
//
// A turn row is a DERIVED, consumer-safe view of the run: the
// renderable user query, the assistant answer (inline text below the
// heavy-content threshold, or an artifact reference for heavy
// answers), input/output attachment METADATA (never bytes), the
// lifecycle state, agent binding, token/cost usage, bounded ordered
// reasoning steps, bounded content-free activity rows, and MCP App
// references with component availability. The projection NEVER
// reconstructs the raw conversation history: reads serve the
// projection only, and the durable event log (`state.history`)
// remains the raw-history home.
//
// # DTO families — consumer-safe vs operations-safe (binding)
//
// The package deliberately separates two DTO families:
//
//   - CONSUMER-SAFE DTOs (`TurnRow` and its component types, row.go)
//     are the read surface `sessions.turns.list/get` will project.
//     They carry only derived content: rendered query, inline/reference
//     answer, attachment metadata, lifecycle/agent/usage, bounded
//     ordered reasoning and activity, and App refs plus availability.
//     They never carry raw tool arguments/results, raw transcripts,
//     App-correlation tokens, or pause/resume tokens.
//
//   - OPERATIONS-SAFE DTOs (`ops.Append`, `ops.Update`, `ops.Seal`,
//     ops.go) are the projector's mutation surface. They are
//     STRUCTURALLY unable to contain transcript, reasoning,
//     App-correlation, or pause tokens: the types have no fields for
//     them, and an allowlist pin test (ops_safety_test.go) holds the
//     field set to the documented allowlist, so a future content field
//     cannot be added silently. The two components a consumer row does
//     carry — reasoning steps and App refs — are written ONLY through
//     their separately named attach channels (`AttachReasoning`,
//     `AttachAppRef`), never through the generic ops.
//
// # Ordering, paging, retention
//
// Every turn gets an IMMUTABLE per-session sequence at append time
// (minted atomically by the Store) plus an immutable tie-breaker
// (the turn id). Listing is newest-first keyset paging over
// `(sequence DESC, turn id DESC)`: because the ordering keys never
// change, a page cursor is stable under concurrent appends — a newly
// appended turn can never satisfy an already-issued cursor, and an
// already-returned turn can never be returned again (no skips, no
// duplicates). The projection is BOUNDED: each session retains only
// the newest `MaxRetainedTurns` rows (older turns live on in the
// durable event log and are evicted from the projection, with the
// truncation made explicit on the page). This is a projection, not a
// warehouse.
//
// # Lifecycle of a row
//
// A turn starts MUTABLE (status `running` or `paused`) and versioned:
// the runtime updates it in place (answer, usage, activity, ...),
// each accepted write bumping `Version`. It becomes a SEALED terminal
// row (`complete` / `failed` / `cancelled`) through `Seal`, which
// refuses until the terminal status's REQUIRED sources are present
// (a `complete` seal requires the answer component; a `failed` seal
// requires the error class). Sealed rows are immutable: the store
// refuses every later mutation.
//
// # Restart, reconcile, erasure
//
// The projector maintains a per-session MONOTONIC, IDEMPOTENT durable
// checkpoint of the last-applied runtime event sequence. `Reconcile`
// replays observations past the checkpoint; re-applying an
// already-applied observation is a no-op (appends are idempotent on
// the turn id, updates/seals fail with a stale version the replay
// treats as "already applied"). An IN-MEMORY-backed store reports
// `Durable() == false`: after a process restart its projection is
// EMPTY (rows AND checkpoint gone — explicit loss, never a silent
// claim of durability) and the runtime rebuilds it by reconciling
// from sequence zero against the durable event log.
//
// Every turn write is fenced against session erasure (right to
// erasure, RFC §7): the write composes expectations that the
// session's pending-erasure ledger and terminal erasure tombstone
// slots are ABSENT. Once an erasure has begun or converged, no turn
// write is admitted (`ErrErasureFenced`); `Erase` / `Store.DeleteScope`
// remove the projection's rows with the erasure cascade.
//
// # Scope of this package
//
// This package ships the domain types, the mandatory conformance-ready
// `Store` interface, the `Projector` core, and the conformance suite
// (`internal/sessions/turns/conformancetest`). It ships NO concrete
// persistent driver: drivers (in-memory, SQLite, Postgres) land in a
// later lane and must pass the conformance suite. The runtime event →
// observation mapping and the Protocol surface are also out of scope.
package turns

import (
	"errors"
	"time"
)

// TurnID identifies one turn — the root foreground run's task id,
// carried as an opaque string. Unique within the session's identity
// triple; the store keys every turn row by `(triple, TurnID)`.
type TurnID string

// Seq is the immutable per-session ordering key minted for a turn at
// append time. It is assigned atomically by the Store (never by the
// caller), is unique within the session, and never changes for the
// lifetime of the row — that immutability is what makes keyset
// paging stable under concurrent appends.
type Seq int64

// Status is the turn row's lifecycle state. Rows in StatusRunning or
// StatusPaused are MUTABLE and versioned; rows in a terminal status
// are SEALED and immutable.
type Status string

// Turn lifecycle statuses.
const (
	// StatusRunning — the run is actively executing. Mutable.
	StatusRunning Status = "running"
	// StatusPaused — the run is paused (HITL approval, OAuth, A2A
	// INPUT_REQUIRED, steering PAUSE — the unified pause/resume
	// primitive). Mutable. The row deliberately carries NO pause or
	// resume token: the projection must never become a pause-token
	// warehouse, and resuming is the runtime's concern.
	StatusPaused Status = "paused"
	// StatusComplete — the run finished successfully (a terminal
	// `task.completed`). SEALED: immutable once sealed.
	StatusComplete Status = "complete"
	// StatusFailed — the run failed (a terminal `task.failed`).
	// SEALED: immutable once sealed.
	StatusFailed Status = "failed"
	// StatusCancelled — the run was cancelled (a terminal
	// `task.cancelled`). SEALED: immutable once sealed.
	StatusCancelled Status = "cancelled"
)

// Mutable reports whether s is a mutable (running / paused) status.
func (s Status) Mutable() bool { return s == StatusRunning || s == StatusPaused }

// Terminal reports whether s is a sealed terminal status.
func (s Status) Terminal() bool {
	return s == StatusComplete || s == StatusFailed || s == StatusCancelled
}

// Valid reports whether s is one of the declared statuses.
func (s Status) Valid() bool { return s.Mutable() || s.Terminal() }

// Completeness is the honest state of one row component. Every
// component of a TurnRow carries one; an absent source is
// `Unavailable` (an honest "we don't have this data"), never a
// fabricated value or a silent zero (CLAUDE.md §13).
type Completeness string

// Component completeness states.
const (
	// CompletenessComplete — the component is fully present.
	CompletenessComplete Completeness = "complete"
	// CompletenessPartial — the component is present but truncated
	// (bounded reasoning/activity windows dropped some entries).
	CompletenessPartial Completeness = "partial"
	// CompletenessUnavailable — no source reported this component.
	CompletenessUnavailable Completeness = "unavailable"
)

// Valid reports whether c is one of the declared completeness states.
func (c Completeness) Valid() bool {
	switch c {
	case CompletenessComplete, CompletenessPartial, CompletenessUnavailable:
		return true
	}
	return false
}

// ActivityStatus is the content-free lifecycle of one activity row
// (one tool dispatch within the turn).
type ActivityStatus string

// Activity row statuses.
const (
	// ActivityInvoked — the dispatch is in flight.
	ActivityInvoked ActivityStatus = "invoked"
	// ActivitySucceeded — the dispatch completed.
	ActivitySucceeded ActivityStatus = "succeeded"
	// ActivityFailed — the dispatch failed.
	ActivityFailed ActivityStatus = "failed"
)

// Valid reports whether s is one of the declared activity statuses.
func (s ActivityStatus) Valid() bool {
	switch s {
	case ActivityInvoked, ActivitySucceeded, ActivityFailed:
		return true
	}
	return false
}

// AppAvailability is the component availability of one MCP App
// reference on a turn. A replayed (read-only) projection must never
// rehydrate live callback authority, so availability is carried
// explicitly: a ref whose persisted tool context can no longer be
// resolved renders an honest "no longer available" placeholder rather
// than mounting broken.
type AppAvailability string

// App component availability states.
const (
	// AppAvailable — the App's tool context resolves; the host can
	// mount it live.
	AppAvailable AppAvailability = "available"
	// AppUnavailable — the App's persisted tool context cannot be
	// resolved; render the honest placeholder.
	AppUnavailable AppAvailability = "unavailable"
	// AppDegraded — the App reference is present but some required
	// dependency (server, resource) is missing; render degraded.
	AppDegraded AppAvailability = "degraded"
)

// Valid reports whether a is one of the declared availability states.
func (a AppAvailability) Valid() bool {
	switch a {
	case AppAvailable, AppUnavailable, AppDegraded:
		return true
	}
	return false
}

// Projection bounds. These are the projection core's choke points:
// everything the row carries is bounded, and an over-bound input
// fails loud (ErrInvalidInput) rather than being silently truncated —
// except the reasoning / activity / attachment windows, whose overflow
// is the explicit lower-bound / partial contract (see row.go).
const (
	// MaxListLimit bounds one List page so no caller can turn the
	// paged read into an accidental unbounded dump.
	MaxListLimit = 200
	// DefaultListLimit is the page size used when List is called with
	// a zero Limit.
	DefaultListLimit = 50
	// MaxRetainedTurns is the documented default bound on the number
	// of turn rows the projection retains per session. Older turns are
	// evicted from the projection (they remain in the durable event
	// log) and the eviction is surfaced as the page Truncated flag.
	// A Store implementation is configured with its own bound; this is
	// the default the reference shape documents.
	MaxRetainedTurns = 200
	// MaxReasoningSteps bounds the ordered reasoning steps a turn row
	// carries. Overflow keeps the FIRST MaxReasoningSteps (the
	// chronological sequence) and marks the component Partial with the
	// dropped count — reasoning overflow is a partial-state, not a
	// silent truncation.
	MaxReasoningSteps = 32
	// MaxActivityRows bounds the activity rows a turn row carries.
	// Overflow keeps the LAST MaxActivityRows (the recent window) and
	// marks the component with the explicit lower-bound More flag plus
	// the dropped count.
	MaxActivityRows = 32
	// MaxAttachmentsPerSide bounds the input / output attachment
	// metadata lists.
	MaxAttachmentsPerSide = 32
	// MaxQueryRunes bounds the renderable query text (runes,
	// post-validation). An over-bound query fails loud.
	MaxQueryRunes = 32 * 1024
	// MaxInlineAnswerBytes bounds the inline answer text. An inline
	// answer at or above the bound is refused with ErrContextLeak — the
	// runtime must route heavy answers through the artifact store by
	// reference (D-026 heavy-content discipline mirrored at the
	// projection edge; the default matches the runtime heavy-output
	// threshold).
	MaxInlineAnswerBytes = 32 * 1024
	// MaxActivitySummaryRunes bounds one activity row's content-free
	// summary (duration / error class — never raw arguments or
	// results).
	MaxActivitySummaryRunes = 512
	// MaxModelRunes bounds the usage model identifier.
	MaxModelRunes = 256
	// MaxStepTraceRunes bounds one reasoning step's provider thinking
	// trace. A trace beyond the bound fails loud (ErrInvalidInput) — a
	// single step must fit the bounded row.
	MaxStepTraceRunes = 16 * 1024
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrTurnNotFound — Get/Update/Seal named a turn the projection
	// does not retain (never created, already evicted, or erased).
	ErrTurnNotFound = errors.New("turns: turn not found")
	// ErrTurnSealed — Update/Seal/Attach targeted a SEALED (immutable
	// terminal) row. Sealed rows are never mutated.
	ErrTurnSealed = errors.New("turns: turn is sealed — immutable")
	// ErrStaleVersion — Update/Seal/Attach carried an expected version
	// that does not match the stored row's current version. A
	// concurrent write won the race; the caller reloads and retries
	// (a replay of an already-applied observation treats this as
	// "already applied").
	ErrStaleVersion = errors.New("turns: stale version — row changed concurrently")
	// ErrNotTerminal — Seal was called with a non-terminal status.
	ErrNotTerminal = errors.New("turns: seal requires a terminal status")
	// ErrSealIncomplete — Seal was refused because a required source
	// for the terminal status is missing (a `complete` seal requires
	// the answer component; a `failed` seal requires the error class).
	// The error message names the missing source.
	ErrSealIncomplete = errors.New("turns: seal refused — required source missing")
	// ErrInvalidStatus — a mutation carried a status the row model
	// cannot express (an unknown enum, or a terminal status on
	// Append/Update).
	ErrInvalidStatus = errors.New("turns: invalid status")
	// ErrErasureFenced — a turn write was refused because the session's
	// pending-erasure ledger or terminal erasure tombstone slot is
	// present: an erasure has begun or converged and no turn write is
	// admitted (CLAUDE.md §6 rule 5 / right to erasure).
	ErrErasureFenced = errors.New("turns: session erasure in progress or converged — write refused")
	// ErrInvalidInput — a mutation carried structurally invalid or
	// over-bound input (empty turn id, over-bound query, invalid
	// attachment, ...). Fails loud; never a silent clamp or drop.
	ErrInvalidInput = errors.New("turns: invalid input")
	// ErrContextLeak — an inline answer met or exceeded
	// MaxInlineAnswerBytes. Heavy answers MUST route through the
	// artifact store by reference; an inline heavy answer is a leak
	// and is refused loudly, never truncated (D-026).
	ErrContextLeak = errors.New("turns: heavy answer reached the row as raw inline bytes — route by artifact reference")
	// ErrInvalidCursor — a List cursor was malformed, version-mismatched,
	// or otherwise unreadable. Never silently resets to page one.
	ErrInvalidCursor = errors.New("turns: invalid or unreadable page cursor")
	// ErrStoreClosed — any operation called after Close.
	ErrStoreClosed = errors.New("turns: store is closed")
	// ErrIdentityRequired — an operation carried an identity triple
	// missing one of (tenant, user, session). Identity is mandatory
	// (CLAUDE.md §6).
	ErrIdentityRequired = errors.New("turns: identity triple incomplete")
)

// Clock abstracts time so the projector's timestamps are
// deterministic in tests without time.Sleep. Production code uses
// realClock; tests inject a controllable clock via WithClock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
