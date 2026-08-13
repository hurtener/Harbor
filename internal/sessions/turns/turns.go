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
// the row key IS the run's task id, carried as an opaque string, and
// the row carries the AUTHORITATIVE TaskID plus the ACTUAL runtime
// RunID as distinct fields: a legacy missing run id is explicit
// unavailable, never silently equated with the task id).
//
// A turn row is a DERIVED, consumer-safe view of the run: the
// renderable user query, the assistant answer (inline text below the
// heavy-content threshold, or an artifact reference), input/output
// attachment METADATA (never bytes), the lifecycle state, agent
// binding with its honest provenance, token/cost usage, bounded ordered
// reasoning steps, bounded content-free activity rows, an ordered MCP
// App reference collection with component availability, and the durable
// pause component. The projection NEVER reconstructs the raw
// conversation history: reads serve the projection only, and the
// durable event log (`state.history`) remains the raw-history home.
//
// # DTO families — consumer read, operations read, mutation (binding)
//
// The package deliberately separates THREE DTO families:
//
//   - CONSUMER-SAFE READ (`TurnRow` and its component types, row.go)
//     is the read surface `sessions.turns.list/get` will project. It
//     carries only derived content: rendered query, inline/reference
//     answer, attachment metadata, lifecycle/agent/usage, bounded
//     ordered reasoning and activity, an ORDERED collection of MCP App
//     references with availability, and the durable pause component.
//     It never carries raw tool arguments/results, raw transcripts, or
//     pause/resume/approval tokens.
//
//   - OPERATIONS-SAFE READ (`OpsTurnRow`, row.go) is a pure,
//     STRUCTURALLY DISTINCT read projection the operations surface
//     reads when it must not see consumer transcript content. It
//     retains lifecycle / agent binding / timing / usage / cost /
//     content-free tool activity / counts / App availability summaries
//     / pause class-reason-lifecycle, and structurally omits the query,
//     the answer, reasoning traces, pause tokens, the App resource URI
//     and tool_call_id, and App context/input/result. `Projector.OpsTurn`
//     serves it; the future operations Protocol lane consumes it.
//
//   - MUTATION DTOs (`ops.Append`, `ops.Update`, `ops.Seal`, ops.go)
//     are the projector's WRITE surface. They are NOT the operations
//     read projection and do not satisfy any consumer-vs-operations
//     authority matrix: the authority matrix is about READ projections
//     (TurnRow vs OpsTurnRow above). The mutation DTOs are minimal
//     write shapes — structurally free of transcript, reasoning traces,
//     App correlation, and pause/resume/approval tokens — because a
//     write channel that could carry raw content would let the runtime
//     wiring leak it into the projection, and an allowlist pin test
//     (ops_safety_test.go) holds their field sets to the documented
//     allowlist so no content field can be added silently. The two
//     content-bearing components — reasoning steps and App refs — are
//     written ONLY through their separately named attach channels
//     (`AttachReasoning`, `AttachAppRefs`), never through the generic
//     ops.
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
// duplicates). The public page default/max are 20/50. The opaque
// cursor BINDS its owning session and the projection snapshot
// (as-of retention generation) it was minted against: a
// foreign-session cursor, a stale-snapshot cursor, and an
// expired/retention cursor are each rejected with a DISTINCT domain
// error (ErrCursorForeignSession / ErrCursorSnapshotStale /
// ErrCursorExpired) for Protocol mapping. Each page exposes its
// snapshot as-of, the next older cursor, has_more, the exact
// older-row remaining count when known, the explicit completeness /
// partial reason, and a live-resume sequence sufficient to compose
// subscribe-before-page. The projection is BOUNDED: each session
// retains only the newest `MaxRetainedTurns` rows (older turns live
// on in the durable event log and are evicted from the projection,
// with the truncation made explicit on the page). This is a
// projection, not a warehouse.
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
// checkpoint of the last-applied runtime event sequence, and every
// observation carries its event sequence. `Reconcile` replays
// observations past the checkpoint; applying an observation AT OR
// BELOW a row's last-applied sequence is a NO-OP (a response-loss
// replay or an out-of-order feed never mutates the row and never
// needs a lucky expected version), and the row's LastAppliedEventSeq
// and the accumulated Answer/Reasoning snapshot sequences NEVER
// regress. An IN-MEMORY-backed store reports `Durable() == false`:
// after a process restart its projection is EMPTY (rows, checkpoint,
// AND erasure fence gone — explicit loss, never a silent claim of
// durability) and the runtime rebuilds it by reconciling from
// sequence zero against the durable event log — a rebuild gated on
// the runtime's DURABLE erasure probe (ErasureProbe), so an erased
// session is never rebuilt merely because the in-memory store
// restarted. A DURABLE driver's store-local erasure fence survives
// restarts permanently.
//
// Every turn write is fenced against session erasure (right to
// erasure, RFC §7) by a STORE-LOCAL durable session fence: `Erase`
// (and the cascade's `Store.FenceSession`) sets the session's fence
// in the driver's OWN backend BEFORE any row is deleted, and every
// write — Append / Update / Seal / checkpoint — atomically refuses a
// fenced session (`ErrErasureFenced`). The fence is never removed by
// the erasure itself, so an erased session stays fenced across replay
// and restart (no resurrection), and re-erase is idempotent. The fence
// is deliberately store-local: a driver cannot transactionally inspect
// arbitrary external StateStore slots, so the fence lives in the same
// transaction as the rows it guards. This package invents no
// cross-runtime authority — the runtime's shared erasure ledger stays
// the erasure cascade's own coordination surface; where a restart
// loses the local fence (in-memory backing), the runtime's durable
// erasure authority is consulted via the ErasureProbe the projector
// holds, and the honest availability gap of a nil probe is documented,
// never silently closed.
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
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
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

// AgentBindingSource is the honest provenance of the agent binding a
// turn executed under. The wire contract requires the source to be one
// of the three declared values; it is never derived into something
// stronger than what the runtime reported.
type AgentBindingSource string

// Agent binding provenance states.
const (
	// AgentBindingExplicit — the run was bound to a named registered
	// agent by an explicit routing decision.
	AgentBindingExplicit AgentBindingSource = "explicit"
	// AgentBindingDefaulted — the run executed under the runtime's
	// default agent binding (no explicit choice).
	AgentBindingDefaulted AgentBindingSource = "defaulted"
	// AgentBindingUnknown — no binding provenance was reported (the
	// honest "we don't know", never a fabricated explicit claim).
	AgentBindingUnknown AgentBindingSource = "unknown"
)

// Valid reports whether s is one of the declared binding sources.
func (s AgentBindingSource) Valid() bool {
	switch s {
	case AgentBindingExplicit, AgentBindingDefaulted, AgentBindingUnknown:
		return true
	}
	return false
}

// AnswerState is the CLOSED UNION describing what the answer component
// carries. The wire contract reads exactly one of the five states; the
// projection never invents a state a failed read would mask — a read
// failure surfaces as Evicted or Unavailable, NEVER as Empty (an Empty
// answer is a definite "the run produced no text", not a failure
// artifact).
type AnswerState string

// Answer component states.
const (
	// AnswerStateInline — the answer is inline text (possibly "" when
	// the model finished with goal and produced no text).
	AnswerStateInline AnswerState = "inline"
	// AnswerStateArtifactRef — the answer is heavy and routed through
	// the artifact store by reference.
	AnswerStateArtifactRef AnswerState = "artifact_ref"
	// AnswerStateEmpty — the run produced a definite answer with no
	// text and no artifact (e.g. a tool-only completion).
	AnswerStateEmpty AnswerState = "empty"
	// AnswerStateEvicted — an artifact-referenced answer whose stored
	// content has been evicted / garbage-collected; the reference is
	// honestly gone, never shown as Empty.
	AnswerStateEvicted AnswerState = "evicted"
	// AnswerStateUnavailable — the run has not produced an answer yet
	// (a running turn), or no answer source was wired.
	AnswerStateUnavailable AnswerState = "unavailable"
)

// Valid reports whether s is one of the declared answer states.
func (s AnswerState) Valid() bool {
	switch s {
	case AnswerStateInline, AnswerStateArtifactRef, AnswerStateEmpty,
		AnswerStateEvicted, AnswerStateUnavailable:
		return true
	}
	return false
}

// PauseClass is the class of one durable pause episode on a row, drawn
// from the producers of the unified pause/resume primitive (HITL
// approval, tool-side OAuth, A2A AUTH_REQUIRED / INPUT_REQUIRED,
// steering PAUSE, operator / Console PAUSE).
type PauseClass string

// Pause episode classes.
const (
	PauseClassHitlApproval     PauseClass = "hitl_approval"
	PauseClassOAuth            PauseClass = "oauth"
	PauseClassA2AAuthRequired  PauseClass = "a2a_auth_required"
	PauseClassA2AInputRequired PauseClass = "a2a_input_required"
	PauseClassSteering         PauseClass = "steering"
	PauseClassOperator         PauseClass = "operator"
)

// Valid reports whether c is one of the declared pause classes.
func (c PauseClass) Valid() bool {
	switch c {
	case PauseClassHitlApproval, PauseClassOAuth, PauseClassA2AAuthRequired,
		PauseClassA2AInputRequired, PauseClassSteering, PauseClassOperator:
		return true
	}
	return false
}

// PauseLifecycle is the durable lifecycle state of one pause episode.
type PauseLifecycle string

// Pause episode lifecycle states.
const (
	// PauseLifecycleRequested — a pause was requested but the run has
	// not yet quiesced.
	PauseLifecycleRequested PauseLifecycle = "requested"
	// PauseLifecycleActive — the run is paused (the row's Status is
	// StatusPaused).
	PauseLifecycleActive PauseLifecycle = "active"
	// PauseLifecycleResolved — the pause episode ended (the run
	// resumed or reached a terminal status).
	PauseLifecycleResolved PauseLifecycle = "resolved"
)

// Valid reports whether l is one of the declared pause lifecycle
// states.
func (l PauseLifecycle) Valid() bool {
	switch l {
	case PauseLifecycleRequested, PauseLifecycleActive, PauseLifecycleResolved:
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
	// paged read into an accidental unbounded dump. The PUBLIC list
	// surface default/max are 20/50 (Protocol-mandated).
	MaxListLimit = 50
	// DefaultListLimit is the page size used when List is called with
	// a zero Limit. EXACTLY 20 — the Protocol-mandated public default.
	DefaultListLimit = 20
	// MaxRetainedTurns is the documented default bound on the number
	// of turn rows the projection retains per session. Older turns are
	// evicted from the projection (they remain in the durable event
	// log) and the eviction is surfaced on the page as the explicit
	// completeness / partial reason (Complete=false,
	// PartialReason="retention_eviction").
	// A Store implementation is configured with its own bound; this is
	// the default the reference shape documents.
	MaxRetainedTurns = 200
	// MaxReasoningSteps bounds the ordered reasoning steps a turn row
	// carries. Overflow keeps the FIRST MaxReasoningSteps (the
	// chronological sequence) and marks the component Partial with the
	// dropped count — reasoning overflow is a partial-state, not a
	// silent truncation.
	MaxReasoningSteps = 32
	// MaxActivityRows is the safe absolute PROTOCOL CEILING on one turn
	// row's inline activity window: no response may carry more activity
	// rows than this, no matter how the projector is configured. The
	// projector's own inline limit is configured (WithActivityLimit),
	// must be >= the runtime's configured per-turn tool-call budget
	// (WithToolBudget), and is capped at this ceiling. A turn whose
	// actual tool calls exceed the configured window overflows honestly
	// (More + Dropped + Partial) and the full activity is read through
	// the named bounded ActivityReader — never a generic subresource.
	MaxActivityRows = 128
	// DefaultActivityLimit is the projector's default inline activity
	// window (WithActivityLimit when none is configured).
	DefaultActivityLimit = 32
	// DefaultToolBudget is the default runtime-configured per-turn
	// tool-call budget the projector validates its inline limit against
	// (WithToolBudget when none is configured). Construction fails loud
	// when the configured limit is below the configured budget WHILE
	// that budget is at or below the Protocol ceiling; a budget ABOVE
	// the ceiling is served by the capped inline window plus the named
	// bounded ActivityReader fallback (never a construction failure).
	DefaultToolBudget = 16
	// MaxAttachmentsPerSide bounds the input / output attachment
	// metadata lists.
	MaxAttachmentsPerSide = 32
	// MaxAppsPerTurn bounds the turn's ORDERED MCP App reference
	// collection. An App ref is a declaration, not a window: an over-
	// bound feed fails loud (ErrInvalidInput) — Apps are never silently
	// dropped or truncated.
	MaxAppsPerTurn = 32
	// MaxToolNameRunes bounds a tool-name field (activity rows and App
	// refs' originating tool name).
	MaxToolNameRunes = 256
	// App string-field bounds (runes, post-UTF-8 validation). Every
	// AppRef string/URI/tool field is bounded, valid UTF-8, and free of
	// NUL / C0-control / DEL ambiguity (see projector.validateText).
	MaxAppAgentIDRunes     = 256
	MaxAppServerIDRunes    = 256
	MaxAppResourceURIRunes = 2048
	MaxAppDisplayModeRunes = 64
	MaxAppToolCallIDRunes  = 256
	// MaxQueryRunes bounds the renderable query text (runes,
	// post-validation). An over-bound query fails loud.
	MaxQueryRunes = 32 * 1024
	// MaxInlineAnswerBytes bounds the inline answer text. An inline
	// answer at or above the bound is refused with ErrContextLeak — the
	// runtime must route heavy answers through the artifact store by
	// reference (the heavy-content discipline mirrored at the
	// projection edge; the default matches the runtime heavy-output
	// threshold).
	MaxInlineAnswerBytes = 32 * 1024
	// MaxActivitySummaryRunes bounds one activity row's content-free
	// summary (duration / error class — never raw arguments or
	// results).
	MaxActivitySummaryRunes = 512
	// MaxActivityPageSize bounds one ActivityReader page. A larger
	// limit fails loud (ErrInvalidInput) — bounded paging, never an
	// accidental dump of a whole over-budget turn.
	MaxActivityPageSize = MaxActivityRows
	// MaxPauseReasonRunes bounds the pause component's content-free
	// reason text (a short derived string — never a token, never raw
	// approval context).
	MaxPauseReasonRunes = 512
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
	// STORE-LOCAL durable erasure fence is present: an erasure has
	// begun or converged and no turn write is admitted (CLAUDE.md §6
	// rule 5 / right to erasure). Also raised by Reconcile when the
	// runtime's durable erasure probe reports the session erased and
	// the store-local fence is gone (an in-memory-backed store after a
	// process restart) — an erased session never rebuilds from
	// sequence zero.
	ErrErasureFenced = errors.New("turns: session erasure in progress or converged — write refused")
	// ErrInvalidInput — a mutation carried structurally invalid or
	// over-bound input (empty turn id, over-bound query, invalid
	// attachment, ...). Fails loud; never a silent clamp or drop.
	ErrInvalidInput = errors.New("turns: invalid input")
	// ErrContextLeak — an inline answer met or exceeded
	// MaxInlineAnswerBytes. Heavy answers MUST route through the
	// artifact store by reference; an inline heavy answer is a leak
	// and is refused loudly, never truncated.
	ErrContextLeak = errors.New("turns: heavy answer reached the row as raw inline bytes — route by artifact reference")
	// ErrInvalidCursor — a List cursor was malformed, version-mismatched,
	// or otherwise unreadable. Never silently resets to page one.
	ErrInvalidCursor = errors.New("turns: invalid or unreadable page cursor")
	// ErrCursorForeignSession — a List cursor was minted for a DIFFERENT
	// session than the request's identity triple. The cursor is rejected
	// (never silently re-scoped); distinct from ErrInvalidCursor so the
	// Protocol layer maps it onto its own domain error.
	ErrCursorForeignSession = fmt.Errorf("%w (foreign session)", ErrInvalidCursor)
	// ErrCursorSnapshotStale — a List cursor's projection snapshot
	// generation no longer matches the session's current snapshot (the
	// projection was erased / rebuilt underneath the walk, so the
	// cursor's as-of retention generation is gone). The caller restarts
	// the walk from page one.
	ErrCursorSnapshotStale = fmt.Errorf("%w (stale projection snapshot)", ErrInvalidCursor)
	// ErrCursorExpired — a List cursor's boundary row is no longer
	// retained (evicted past the retention bound, or never existed).
	// The projection honestly no longer pages from that position.
	ErrCursorExpired = fmt.Errorf("%w (boundary row no longer retained)", ErrInvalidCursor)
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

// ErasureProbe is the runtime-side DURABLE erasure check the projector
// consults during RESTART RECONCILIATION. An in-memory-backed store
// (Durable() == false) loses its STORE-LOCAL erasure fence on a
// process restart — rows, checkpoint, AND fence are all gone — so the
// runtime's own durable erasure cascade / fence (the pending-erasure
// ledger and terminal tombstone under the observability scope,
// `internal/sessions`) is the ONLY authority that can still tell "this
// session was erased" from "this session never existed". The runtime
// wires the probe over that cascade; this package never inspects
// external StateStore slots itself (store-local fence only — no
// cross-runtime authority).
//
// A nil probe means the runtime declared no durable erasure authority:
// Reconcile then rebuilds on the store-local fence alone, which for an
// in-memory-backed store after a restart is an HONEST availability gap
// (an erased session COULD be rebuilt from sequence zero — the loss is
// explicit, never claimed otherwise). Runtimes with a durable erasure
// cascade MUST wire the probe so that gap is closed (P6 / G4).
type ErasureProbe interface {
	// Erased reports whether the runtime's durable erasure cascade /
	// fence has erased (or is in the process of erasing) the session.
	Erased(ctx context.Context, id identity.Identity) (bool, error)
}
