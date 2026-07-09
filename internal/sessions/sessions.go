// Package sessions owns Harbor's session-lifecycle subsystem.
//
// A Session is a longer-lived multi-turn conversation that contains
// many Runs. Identity for runtime concerns is the triple
// `(tenant, user, session)`; runs are scoped within sessions
// (RFC §6.9). The Session record itself is keyed in the StateStore
// at `Kind = "session.lifecycle"` with `RunID = ""` — sessions are
// session-scoped, not run-scoped.
//
// Harbor ships a single concrete *Registry implementation that sits
// over the StateStore, codifying the typed-wrapper-over-generic
// StateStore contract. There is no driver pluralism at the session
// layer; driver pluralism lives at StateStore (in-mem / SQLite / Postgres
// already). Per AGENTS.md §4.4, optional-capability
// ceremony is forbidden when all V1 drivers (here: implementations)
// will implement everything.
//
// Four lifetime invariants are load-bearing and pinned by tests:
//
//  1. Identity captured immutably on Open — Touch / Close re-save
//     the same identity from the existing record; mismatched ctx
//     identity is rejected with ErrIdentityMismatch.
//  2. Reopen-after-close forbidden — clients open a new SessionID.
//  3. Cross-tenant SessionID reuse rejected — `SessionID=S` opened
//     under Tenant A then attempted under Tenant B returns
//     ErrSessionIDReuse, even though the StateStore key (which
//     contains the full Quadruple) would not naturally collide.
//  4. GC never reaps a session with a RUNNING task — enforced when
//     the TaskRegistry-backed probe (TaskRunningProbe) is wired via
//     WithGCPolicy, as both reference assemblies do. The no-op
//     default (returns false, nil) exists only for registries
//     constructed without task awareness.
//
// Lifecycle events (`session.opened / .touched / .closed / .gc_reaped`)
// land on the EventBus as `SafePayload` types — they're Harbor-internal
// markers with no secret-shaped fields by construction (RFC §6.13).
// Subscribers can extract typed fields directly, no redactor
// walk in between.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
)

// Session is the persisted lifecycle record for one session. The
// Identity field carries the triple captured on Open and is immutable
// afterwards. Closed transitions to true on Close; ClosedAt stays zero
// while Closed is false.
//
// Title / TitleSource are additive fields: an older persisted session
// record (one written before these fields existed) decodes with both
// zero-valued (""), which is exactly TitleSourceUnset — no migration
// needed. Title is set/cleared exclusively through SetTitle, which
// enforces the manual semantics (trim, length/control-character
// validation, empty clears both fields). Title is user-derived free
// text and MUST NEVER be copied into an event/log/audit payload
// (CLAUDE.md §7 rule 7 — content-free posture) — only SessionID +
// TitleSource ride SessionTitleChangedPayload.
//
// Limits and Context are reserved slots — later phases will populate
// Limits with the cost / token ceilings and
// Context with the (version, hash, llm/tool ctx, memory, artifacts)
// quintuple sketched in RFC §6.9. round-trips both fields
// through marshal/unmarshal but applies no validation.
type Session struct {
	ID           string
	Identity     identity.Identity
	OpenedAt     time.Time
	LastSeen     time.Time
	Closed       bool
	ClosedAt     time.Time
	ClosedReason string
	Limits       SessionLimits
	Context      map[string]any
	Title        string
	TitleSource  TitleSource
	// TurnCount / AutoNameCount / LastAutoNamedTurn are the auto-naming
	// counters. They are additive JSON fields: an older persisted record
	// (written before auto-naming existed) decodes with all three
	// zero-valued. They are written ONLY when a naming policy is active for
	// the run (a config-free runtime never touches them), so a naming-off
	// fleet carries no per-run write amplification and the counters read
	// zero for every session. Written exclusively through
	// RecordCompletedTurn (bumps TurnCount) and SetTitleAuto (bumps
	// AutoNameCount + LastAutoNamedTurn alongside the title, in ONE record
	// save).
	//
	// TurnCount counts runs completed while a naming policy was active —
	// counting starts at policy enablement, so enabling naming mid-session
	// counts turns from enablement, not from session open (a documented
	// consequence of writing counters only when the policy is active).
	// AutoNameCount is the number of auto-naming LLM calls that produced a
	// title. LastAutoNamedTurn is the TurnCount at which the most recent
	// auto-naming landed (the re-naming cadence anchor).
	TurnCount         int
	AutoNameCount     int
	LastAutoNamedTurn int
}

// TitleSource marks who produced a session's Title. The wire verb
// `sessions.set_title` ALWAYS writes TitleSourceManual (empty title
// clears both fields to TitleSourceUnset) — TitleSourceAuto is not
// expressible over the wire; it is declared here for the runtime's
// internal auto-naming writer, which is the only producer. This makes
// "manual wins" structurally unforgeable: a caller can never claim
// `auto` provenance for a title they set themselves.
type TitleSource string

const (
	// TitleSourceUnset — no title has ever been set, or the title was
	// cleared (an empty SetTitle call resets both Title and
	// TitleSource to this zero value).
	TitleSourceUnset TitleSource = ""
	// TitleSourceAuto — the title was produced by the runtime's internal
	// auto-naming writer. Not reachable via the `sessions.set_title`
	// wire verb.
	TitleSourceAuto TitleSource = "auto"
	// TitleSourceManual — the title was set by an authenticated caller
	// via `sessions.set_title`. Manual titles are never silently
	// overwritten by an auto-namer.
	TitleSourceManual TitleSource = "manual"
)

// MaxSessionTitleLen bounds a manual session title (runes, post-trim).
// A title exceeding this bound is rejected fail-loud with ErrInvalidTitle
// — never silently clamped (CLAUDE.md §13).
const MaxSessionTitleLen = 200

// ErrInvalidTitle — SetTitle was called with a title that, after
// trimming leading/trailing whitespace, either exceeds
// MaxSessionTitleLen runes or contains a newline/control character.
// Titles are single-line, bounded, display strings.
var ErrInvalidTitle = errors.New("sessions: invalid title")

// ErrManualTitle — SetTitleAuto was refused because the session's current
// TitleSource is TitleSourceManual. The internal auto-naming write path
// NEVER overwrites a human-set title: manual wins, structurally. A caller
// that sees this treats it as a benign skip (the run's outcome is
// unaffected), not a failure.
var ErrManualTitle = errors.New("sessions: auto-naming refused — title is manual")

// validateTitle enforces the manual-title shape: single-line (no
// newline/control characters), at most MaxSessionTitleLen runes, and
// not invisible-only. Callers pass an already-trimmed title; an empty
// string is valid here (SetTitle handles the empty-clears-title
// semantics separately).
//
// Unicode format characters (category Cf — zero-width space U+200B,
// zero-width joiner U+200D, directional marks, ...) are NOT control
// characters (unicode.IsControl is Cc-only), so a title may
// legitimately contain them — emoji ZWJ sequences depend on U+200D.
// The one shape rejected on top of the control-character rule is a
// title made ONLY of format characters and whitespace: it survives
// strings.TrimSpace non-empty but renders as nothing — neither a
// usable display name nor an intentional clear. Invisible-only input
// fails loud with ErrInvalidTitle (invalid, NOT treated as a clear) so
// the caller learns the input was garbage rather than silently losing
// the existing title.
func validateTitle(trimmed string) error {
	if utf8.RuneCountInString(trimmed) > MaxSessionTitleLen {
		return fmt.Errorf("%w: exceeds %d runes", ErrInvalidTitle, MaxSessionTitleLen)
	}
	visible := false
	for _, r := range trimmed {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return fmt.Errorf("%w: contains a newline or control character", ErrInvalidTitle)
		}
		if !unicode.Is(unicode.Cf, r) && !unicode.IsSpace(r) {
			visible = true
		}
	}
	if !visible {
		return fmt.Errorf("%w: contains no visible characters (format characters / whitespace only)", ErrInvalidTitle)
	}
	return nil
}

// SessionLimits is reserved for later phases (cost ceilings, tool
// catalog). Empty at V1; round-trips through marshal.
type SessionLimits struct {
	// Reserved.
}

// SessionSnapshot is the read-side projection returned by Inspect.
// Carries the lifecycle fields plus a Running boolean derived from
// the GCPolicy.RunningProbe at inspection time. Running is intrinsically
// stale by the time the caller reads it; the same is true of any
// snapshot model.
type SessionSnapshot struct {
	Session
	Running bool
}

// AutoNamingState is the read-side projection the auto-naming eligibility
// check consumes: the session's current title provenance plus its three
// naming counters. It is a lightweight read (no lifecycle mutation) so the
// terminal-boundary trigger can decide whether a title is due without a
// second whole-record write.
type AutoNamingState struct {
	// TitleSource is the session's current title provenance — the auto-namer
	// never overwrites TitleSourceManual.
	TitleSource TitleSource
	// CurrentTitle is the session's current title (may be empty). The
	// re-naming path includes it in the digest so the model can refine an
	// existing auto title; it NEVER rides an event payload.
	CurrentTitle string
	// TurnCount / AutoNameCount / LastAutoNamedTurn are the session's naming
	// counters (see Session).
	TurnCount         int
	AutoNameCount     int
	LastAutoNamedTurn int
}

// RunningProbe is the seam the GC sweeper consults so it can honor
// "never reap a session with a RUNNING task" (RFC §6.9). The
// TaskRegistry-backed implementation is TaskRunningProbe; both
// reference assemblies wire it via WithGCPolicy. A nil probe is
// treated as the no-op default (returns false, nil) — only for
// registries constructed without task awareness.
type RunningProbe func(ctx context.Context, q identity.Quadruple) (bool, error)

// GCPolicy bundles the GC sweeper's tunables. Defaults match RFC §6.9:
// IdleTTL 24h, HardCap 720h (30 days), SweepInterval 15m. The
// RunningProbe should be TaskRunningProbe wherever a TaskRegistry is
// in scope; default is the no-op.
type GCPolicy struct {
	IdleTTL       time.Duration
	HardCap       time.Duration
	SweepInterval time.Duration
	RunningProbe  RunningProbe
}

// withDefaults returns a copy of p with zero-valued fields filled
// from the documented RFC §6.9 defaults.
func (p GCPolicy) withDefaults() GCPolicy {
	out := p
	if out.IdleTTL <= 0 {
		out.IdleTTL = 24 * time.Hour
	}
	if out.HardCap <= 0 {
		out.HardCap = 720 * time.Hour
	}
	if out.SweepInterval <= 0 {
		out.SweepInterval = 15 * time.Minute
	}
	if out.RunningProbe == nil {
		out.RunningProbe = func(context.Context, identity.Quadruple) (bool, error) {
			return false, nil
		}
	}
	return out
}

// SessionRegistry is the public surface every consumer (
// envelope writer, Protocol surface, Console
// subscribers, etc.) talks to. One concrete impl ships in a later phase
// (`*Registry`); driver pluralism lives at the StateStore layer.
type SessionRegistry interface {
	Open(ctx context.Context, id string, ident identity.Identity) (*Session, error)
	// EnsureOpen is the create-on-first-use entry point: it
	// returns the live session for ident, creating it if absent and
	// no-opping if already open. A closed session is NOT revived
	// (ErrReopenAfterClose). See the concrete impl for full semantics.
	EnsureOpen(ctx context.Context, ident identity.Identity) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	Touch(ctx context.Context, id string) error
	Close(ctx context.Context, id string, reason string) error
	Inspect(ctx context.Context, id string) (*SessionSnapshot, error)
	GC(ctx context.Context, policy GCPolicy) (int, error)

	// SetTitle sets or clears the manual title of session `id`. `ident`
	// is the CALLER's verified (tenant, user, session) — id MAY name a
	// sibling session of the same (tenant, user) (the write scope is
	// (tenant, user), matching sessions.list); it is not
	// required to equal ident.SessionID. Semantics: trims whitespace;
	// an empty-after-trim title clears Title and resets TitleSource to
	// TitleSourceUnset; a non-empty title is validated (MaxSessionTitleLen,
	// no newline/control characters — ErrInvalidTitle, never a silent
	// clamp) and stored with TitleSource = TitleSourceManual. Unknown id
	// (or an id belonging to a different (tenant, user)) returns
	// ErrSessionNotFound; a stored-identity (tenant, user) mismatch
	// (defence-in-depth) returns ErrIdentityMismatch. Renaming a CLOSED
	// session is allowed; renaming an ERASED session is
	// ErrSessionNotFound (the record is gone).
	SetTitle(ctx context.Context, id string, ident identity.Identity, title string) error

	// RecordCompletedTurn increments the session's TurnCount and returns the
	// new value. It is called from the run loop's terminal boundary ONLY when
	// a naming policy is active for the run — a naming-off runtime never calls
	// it, so it writes nothing for the naming-off fleet. `ident` is the run's
	// verified (tenant, user, session); a stored-identity mismatch returns
	// ErrIdentityMismatch, an unknown / erased id returns ErrSessionNotFound.
	// The load→bump→save runs through the serialized whole-record write path
	// (never a torn two-write update).
	RecordCompletedTurn(ctx context.Context, id string, ident identity.Identity) (int, error)

	// SetTitleAuto sets the session's title from the internal auto-namer and
	// bumps AutoNameCount + LastAutoNamedTurn in the SAME record save. It
	// REFUSES with ErrManualTitle when the current TitleSource is
	// TitleSourceManual (manual wins, structurally). `title` is expected to be
	// already clamped by the caller to the naming policy bound; the registry
	// re-clamps defensively to MaxSessionTitleLen runes and single-line, and
	// rejects an empty-after-trim title with ErrInvalidTitle (the auto path
	// never intentionally clears). On success it stores TitleSource =
	// TitleSourceAuto and publishes a content-free session.title_changed
	// (source=auto). `ident` is the run's verified triple; a stored-identity
	// mismatch returns ErrIdentityMismatch, an unknown / erased id returns
	// ErrSessionNotFound.
	SetTitleAuto(ctx context.Context, id string, ident identity.Identity, title string) error

	// CloseRegistry cancels the sweeper goroutine and joins it.
	// Idempotent. Distinct method name (rather than Close) so it
	// doesn't collide with Close(id, reason).
	CloseRegistry(ctx context.Context) error
}

// SessionLister is the narrow read-side capability the
// `search.sessions` Searcher consumes. The triple `(tenant, user,
// session)` is the load-bearing isolation key (CLAUDE.md §6); the
// listing is server-enforced per the supplied SessionListFilter. The
// returned snapshots include both currently-open and previously-closed
// sessions — search wants the union.
//
// Intentionally NOT on the SessionRegistry interface: the lister is a
// projection over the in-memory open-session index plus the StateStore.
// Concrete `*Registry` implements it; future drivers add it when their
// backing store gains a `list` capability (StateStore List is post-V1).
type SessionLister interface {
	// ListSnapshots returns session snapshots that match the filter,
	// scoped to the requested tenants. Empty `TenantIDs` matches every
	// tenant the registry has seen (the search subsystem gates this
	// on the caller's auth.ScopeAdmin claim — the registry does NOT
	// re-check scope). Empty `UserIDs` / `SessionIDs` are wildcards.
	ListSnapshots(ctx context.Context, f SessionListFilter) ([]SessionSnapshot, error)
}

// SessionListFilter narrows ListSnapshots's result set. All fields are
// wildcards when empty. SinceLastSeen / UntilLastSeen filter by the
// LastSeen timestamp; zero means "no bound."
type SessionListFilter struct {
	TenantIDs     []string
	UserIDs       []string
	SessionIDs    []string
	SinceLastSeen time.Time
	UntilLastSeen time.Time
	IncludeClosed bool
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrReopenAfterClose — Open called for a SessionID whose existing
	// record is Closed. Per RFC §6.9 ("Reopen-after-close is forbidden").
	ErrReopenAfterClose = errors.New("sessions: reopen-after-close forbidden")
	// ErrSessionIDReuse — Open called with a SessionID already opened
	// under a different (tenant, user). Per RFC §6.9 ("reusing a session
	// ID across tenants/users is rejected").
	ErrSessionIDReuse = errors.New("sessions: SessionID reused across tenants/users")
	// ErrIdentityMismatch — Touch / Close called with a ctx Identity
	// that disagrees with the stored session's Identity. The triple is
	// captured immutably on Open; mid-flight identity swaps are bugs.
	ErrIdentityMismatch = errors.New("sessions: ctx identity mismatches stored session identity")
	// ErrSessionNotFound — Get / Touch / Close / Inspect targeting a
	// SessionID that has no record (or the record was Deleted).
	ErrSessionNotFound = errors.New("sessions: session not found")
	// ErrSessionAlreadyOpen — Open called twice with the same triple
	// AND SessionID without an intervening Close. Distinct from
	// ErrReopenAfterClose (which fires when Closed is true).
	ErrSessionAlreadyOpen = errors.New("sessions: session already open")
	// ErrRegistryClosed — any operation called after CloseRegistry.
	ErrRegistryClosed = errors.New("sessions: registry is closed")
	// ErrSessionRunning — Erase was refused because the target session
	// has a RUNNING task. Erasure mirrors the GC never-reap-running
	// invariant (RFC §6.9): a session with in-flight work is durable
	// execution state, not a cache entry, so it is refused fail-loud and
	// NO store is touched. The caller retries after the task finishes.
	ErrSessionRunning = errors.New("sessions: cannot erase a session with a running task")
)

// Clock abstracts time so GC tests are deterministic without
// time.Sleep. Production code uses realClock; tests pass a fakeClock.
//
// The interface intentionally returns time.Time directly (not a
// monotonic count) so GC's wall-clock math is identical between
// production and test paths.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
