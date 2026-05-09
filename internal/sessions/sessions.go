// Package sessions owns Harbor's session-lifecycle subsystem.
//
// A Session is a longer-lived multi-turn conversation that contains
// many Runs. Identity for runtime concerns is the triple
// `(tenant, user, session)`; runs are scoped within sessions
// (RFC §6.9). The Session record itself is keyed in the StateStore
// at `Kind = "session.lifecycle"` with `RunID = ""` — sessions are
// session-scoped, not run-scoped.
//
// Phase 08 ships a single concrete *Registry implementation that sits
// over Phase 07's StateStore, codifying the typed-wrapper-over-generic
// contract from D-027. There is no driver pluralism at the session
// layer; driver pluralism lives at StateStore (in-mem / SQLite / Postgres
// at Phases 07 / 15 / 16). Per AGENTS.md §4.4, optional-capability
// ceremony is forbidden when all V1 drivers (here: implementations)
// will implement everything.
//
// Four lifetime invariants are load-bearing and pinned by tests:
//
//   1. Identity captured immutably on Open — Touch / Close re-save
//      the same identity from the existing record; mismatched ctx
//      identity is rejected with ErrIdentityMismatch.
//   2. Reopen-after-close forbidden — clients open a new SessionID.
//   3. Cross-tenant SessionID reuse rejected — `SessionID=S` opened
//      under Tenant A then attempted under Tenant B returns
//      ErrSessionIDReuse, even though the StateStore key (which
//      contains the full Quadruple) would not naturally collide.
//   4. GC never reaps a session with a RUNNING task — Phase 20
//      (TaskRegistry) wires the real RunningProbe; Phase 08 ships a
//      no-op default that returns (false, nil).
//
// Lifecycle events (`session.opened / .touched / .closed / .gc_reaped`)
// land on the EventBus as `SafePayload` types — they're Harbor-internal
// markers with no secret-shaped fields by construction (RFC §6.13,
// D-028). Subscribers can extract typed fields directly, no redactor
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
// Limits and Context are reserved slots — Phase 36a/b will populate
// Limits with the cost / token ceilings; Phase 23+ will populate
// Context with the (version, hash, llm/tool ctx, memory, artifacts)
// quintuple sketched in RFC §6.9. Phase 08 round-trips both fields
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

// SessionLimits is reserved for Phase 36a (cost ceilings) and Phase 26
// (tool catalog). Empty in Phase 08; round-trips through marshal.
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

// RunningProbe is the seam Phase 20 (TaskRegistry) plugs into so GC
// can honor "never reap a session with a RUNNING task." A nil probe
// is treated as the no-op default (returns false, nil) — Phase 08
// ships no task awareness, and the registry must work without it.
type RunningProbe func(ctx context.Context, q identity.Quadruple) (bool, error)

// GCPolicy bundles the GC sweeper's tunables. Defaults match RFC §6.9:
// IdleTTL 24h, HardCap 720h (30 days), SweepInterval 15m. The
// RunningProbe is the seam Phase 20 plugs into; default is the no-op.
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

// SessionRegistry is the public surface every consumer (Phase 09
// envelope writer, Phase 60 Protocol surface, Phase 72-75 Console
// subscribers, etc.) talks to. One concrete impl ships in Phase 08
// (`*Registry`); driver pluralism lives at the StateStore layer.
type SessionRegistry interface {
	Open(ctx context.Context, id string, ident identity.Identity) (*Session, error)
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
