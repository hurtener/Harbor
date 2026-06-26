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
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Session is the persisted lifecycle record for one session. The
// Identity field carries the triple captured on Open and is immutable
// afterwards. Closed transitions to true on Close; ClosedAt stays zero
// while Closed is false.
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
