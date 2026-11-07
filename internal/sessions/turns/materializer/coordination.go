package materializer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// Coordination defaults. See WithCoordination.
const (
	// defaultLeaseTTL is how long a lease survives without renewal; a
	// crashed holder is replaced within this bound.
	defaultLeaseTTL = 30 * time.Second
	// defaultResumeLookback bounds how far behind the source cursor an
	// open turn may pin the persisted resume cursor (in events). An open
	// turn older than this is not replayed on restart; its later events
	// adopt the durable row instead (see adoptTurn).
	defaultResumeLookback = 10_000
	// defaultRepairBudget bounds the identity repairs one pass attempts.
	defaultRepairBudget = 32
	// repairSettleAge is how long after its last write a sealed row
	// whose task record is still unreadable stops being retried.
	repairSettleAge = time.Hour
	// defaultRepairRescan is how long an exhausted repair scan idles
	// before it re-reads the flagged rows from the start.
	defaultRepairRescan = 5 * time.Minute
	// defaultSpawnDeferAttempts / defaultSpawnDeferGrace bound how long a
	// live root spawn whose task record is not readable yet is deferred
	// before it is appended without its query / agent (and flagged for
	// repair). Both bounds must be exhausted.
	defaultSpawnDeferAttempts = 5
	defaultSpawnDeferGrace    = 10 * time.Second
	// spawnDeferRetry is Run's retry cadence while a spawn is deferred.
	spawnDeferRetry = time.Second
	// maxSpawnDeferrals bounds the deferral bookkeeping; beyond it a
	// missing record is not deferred (the row is flagged for repair).
	maxSpawnDeferrals = 1024
	// maxAdoptMisses bounds a session's negative adoption cache.
	maxAdoptMisses = 4096
	// progressLogEvery bounds the catch-up progress log cadence.
	progressLogEvery = 15 * time.Second
)

// errSpawnDeferred aborts a pass at a live root spawn whose task record
// is not readable yet; the pass resumes from the same event later.
var errSpawnDeferred = errors.New("materializer: root spawn deferred until its task record is readable")

// WithCoordination wires the durable coordination store (normally the
// same driver as the projector's Store). It turns on:
//
//   - the SINGLE-WRITER LEASE: only the holder materializes; another
//     instance sharing the store (a rolling-deploy overlap) stands by
//     and takes over when the holder releases the lease on shutdown or
//     stops renewing it;
//   - the PERSISTED RESUME CURSOR: a restart or takeover pages only the
//     events after the saved cursor instead of the whole log. The saved
//     cursor is held back to just before the oldest still-open turn
//     (within a bounded lookback) so that turn's accumulators rebuild;
//     an older open turn is recovered from its persisted routes, durable
//     row and pending-completion mark;
//   - the bounded IDENTITY REPAIR pass over rows flagged with an
//     unavailable query / agent.
//
// A nil store leaves the materializer uncoordinated (always active,
// in-memory cursor from zero) — the embedded single-instance posture.
func WithCoordination(store turns.MaterializerStore) Option {
	return func(m *Materializer) { m.coord = store }
}

// WithLeaseTTL overrides the lease time-to-live (default 30s; renewal
// runs every third of it). A non-positive value is ignored.
func WithLeaseTTL(d time.Duration) Option {
	return func(m *Materializer) {
		if d > 0 {
			m.leaseTTL = d
		}
	}
}

// WithResumeLookback overrides the bounded open-turn lookback of the
// persisted resume cursor (default 10,000 events). A non-positive value
// is ignored.
func WithResumeLookback(events uint64) Option {
	return func(m *Materializer) {
		if events > 0 {
			m.resumeLookback = events
		}
	}
}

// WithLogger wires the logger for lease transitions and the bounded
// catch-up progress log. Nil keeps the materializer silent.
func WithLogger(l *slog.Logger) Option {
	return func(m *Materializer) { m.logger = l }
}

// withClock is the test seam for the lease / deferral / repair clocks.
func withClock(now func() time.Time) Option {
	return func(m *Materializer) { m.now = now }
}

// withHolder is the test seam for a deterministic lease holder id.
func withHolder(h string) Option {
	return func(m *Materializer) { m.holder = h }
}

// newHolderID mints this instance's lease holder id: host, pid, and
// random entropy (two processes on one host never collide).
func newHolderID() string {
	host, _ := os.Hostname()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return host + "-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b[:])
}

func (m *Materializer) renewInterval() time.Duration { return m.leaseTTL / 3 }

// ensureLeaseLocked reports whether this instance may materialize now.
// Uncoordinated materializers always may. A holder renews once per
// renew interval; a standby retries acquisition at the same cadence (so
// wakes in standby cost nothing). Becoming the holder resets the
// in-memory state and resumes from the persisted cursor; losing the
// lease resets it too. Callers hold mu.
func (m *Materializer) ensureLeaseLocked(ctx context.Context) (bool, error) {
	if m.coord == nil {
		return true, nil
	}
	now := m.now()
	if m.held && now.Sub(m.lastRenew) < m.renewInterval() {
		return true, nil
	}
	if !m.held && !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) < m.renewInterval() {
		return false, nil
	}
	m.lastAttempt = now
	lease, err := m.coord.AcquireMaterializerLease(ctx, m.holder, m.leaseTTL)
	if err != nil {
		return false, fmt.Errorf("materializer: lease: %w", err)
	}
	if !lease.Held {
		if m.held {
			m.loseLeaseLocked(ctx, lease.Holder)
		}
		return false, nil
	}
	if !m.held || lease.Generation != m.fence.Generation {
		// A new tenure (first acquisition, or the lease changed hands in
		// between): whatever this instance held in memory may be stale.
		m.resetStateLocked(lease.Cursor)
		m.held = true
		m.fence = turns.WriteFence{Holder: m.holder, Generation: lease.Generation}
		m.restorePending = true
		m.log(ctx, slog.LevelInfo, "turns materializer acquired lease",
			slog.String("holder", m.holder), slog.Uint64("generation", lease.Generation), slog.Uint64("resume_cursor", lease.Cursor))
	}
	m.lastRenew = now
	return true, nil
}

// loseLeaseLocked steps down to standby: another holder owns the
// projection now, so this instance's in-memory state is discarded and
// rebuilt from the persisted cursor if it ever takes the lease again.
func (m *Materializer) loseLeaseLocked(ctx context.Context, holder string) {
	m.held = false
	m.fence = turns.WriteFence{}
	m.resetStateLocked(0)
	m.log(ctx, slog.LevelWarn, "turns materializer lost lease; standing by",
		slog.String("holder", m.holder), slog.String("current_holder", holder))
}

// resetStateLocked discards every in-memory accumulator and routing
// entry and positions the cursor at the resume point.
func (m *Materializer) resetStateLocked(cursor uint64) {
	m.sessions = map[identity.Identity]*sessionState{}
	m.pending = nil
	m.open = nil
	m.cursor = cursor
	m.resumeFloor = cursor
	m.savedCursor = cursor
	m.spawnMisses = map[spawnKey]*spawnMiss{}
	m.repairAfter = turns.IdentityRepairKey{}
	m.repairIdleUntil = time.Time{}
}

// releaseLease hands the lease over on Run exit (a graceful shutdown
// lets the standby take over immediately instead of after expiry).
func (m *Materializer) releaseLease() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.coord == nil || !m.held {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.coord.ReleaseMaterializerLease(ctx, m.holder); err != nil {
		m.log(ctx, slog.LevelWarn, "turns materializer lease release failed", slog.Any("error", err))
	}
	m.held = false
	m.fence = turns.WriteFence{}
	m.resetStateLocked(0)
}

// fencedCtx binds ctx to the current lease tenure so every projection
// write is refused by the store once the lease changed hands.
func (m *Materializer) fencedCtx(ctx context.Context) context.Context {
	if m.coord == nil || !m.held {
		return ctx
	}
	return turns.WithWriteFence(ctx, m.fence)
}

// leaseLostLocked steps down when err reports a fenced-out write.
func (m *Materializer) leaseLostLocked(ctx context.Context, err error) bool {
	if m.coord == nil || !errors.Is(err, turns.ErrLeaseNotHeld) {
		return false
	}
	if m.held {
		m.loseLeaseLocked(ctx, "")
	}
	return true
}

// leaseTick is Run's lease cadence: renew (or, in standby, try to
// acquire) without paging the source. It reports whether this instance
// just became the holder (the caller then catches up).
func (m *Materializer) leaseTick(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	was := m.held
	held, err := m.ensureLeaseLocked(ctx)
	return held && !was, err
}

// openRef is one turn that may pin the resume cursor.
type openRef struct {
	ts       *turnState
	spawnSeq uint64
}

// resumePointLocked is the cursor a restart may safely resume from:
// the source cursor, held back to just before the oldest non-terminal
// turn spawned within the lookback (so its accumulators and deferred
// seal rebuild by replay). The open list is compacted as it is read.
func (m *Materializer) resumePointLocked() uint64 {
	r := m.cursor
	var floor uint64
	if m.cursor > m.resumeLookback {
		floor = m.cursor - m.resumeLookback
	}
	kept := m.open[:0]
	for _, o := range m.open {
		if o.ts.terminal() || o.spawnSeq <= floor {
			continue
		}
		kept = append(kept, o)
		if o.spawnSeq-1 < r {
			r = o.spawnSeq - 1
		}
	}
	for i := len(kept); i < len(m.open); i++ {
		m.open[i] = openRef{}
	}
	m.open = kept
	return r
}

// persistCursorLocked saves the resume point when it advanced. A lost
// lease steps down (reported as lost=true).
func (m *Materializer) persistCursorLocked(ctx context.Context) (lost bool, err error) {
	if m.coord == nil || !m.held {
		return false, nil
	}
	r := m.resumePointLocked()
	if r <= m.savedCursor {
		return false, nil
	}
	if err := m.coord.SaveMaterializerCursor(m.fencedCtx(ctx), m.holder, r); err != nil {
		if errors.Is(err, turns.ErrLeaseNotHeld) {
			m.loseLeaseLocked(ctx, "")
			return true, nil
		}
		return false, fmt.Errorf("materializer: save resume cursor: %w", err)
	}
	m.savedCursor = r
	return false, nil
}

// ---------------------------------------------------------------------------
// live spawn deferral
// ---------------------------------------------------------------------------

type spawnKey struct {
	id     identity.Identity
	taskID string
}

type spawnMiss struct {
	attempts int
	first    time.Time
}

// deferSpawnLocked records one miss of a live root spawn's task record
// and reports whether the append should wait for it. Both the attempt
// bound and the grace window must be exhausted before the spawn is
// appended without the record; the bookkeeping itself is bounded.
func (m *Materializer) deferSpawnLocked(id identity.Identity, taskID string) bool {
	k := spawnKey{id: id, taskID: taskID}
	miss := m.spawnMisses[k]
	if miss == nil {
		if len(m.spawnMisses) >= maxSpawnDeferrals {
			return false
		}
		miss = &spawnMiss{first: m.now()}
		m.spawnMisses[k] = miss
	}
	miss.attempts++
	if miss.attempts > m.spawnDeferAttempts && m.now().Sub(miss.first) >= m.spawnDeferGrace {
		delete(m.spawnMisses, k)
		return false
	}
	return true
}

func (m *Materializer) clearSpawnMissLocked(id identity.Identity, taskID string) {
	delete(m.spawnMisses, spawnKey{id: id, taskID: taskID})
}

// ---------------------------------------------------------------------------
// adoption of open turns spawned before the resume cursor
// ---------------------------------------------------------------------------

// canAdopt reports whether events may name tasks this instance never
// observed spawning: it resumed from a persisted cursor.
func (m *Materializer) canAdopt() bool { return m.coord != nil && m.resumeFloor > 0 }

// saveRoute persists one task routing binding (coordinated only), so a
// later instance resuming past the spawn still routes exactly.
func (m *Materializer) saveRoute(ctx context.Context, sess *sessionState, r turns.TaskRoute) error {
	if m.coord == nil {
		return nil
	}
	if err := m.coord.SaveTaskRoute(ctx, sess.id, r); err != nil {
		return fmt.Errorf("materializer: save route %s: %w", r.TaskID, err)
	}
	return nil
}

// markPending durably marks a deferred complete seal (coordinated only)
// so a restart or takeover rebuilds the pending queue from the store.
func (m *Materializer) markPending(ctx context.Context, sess *sessionState, ts *turnState) error {
	if m.coord == nil {
		return nil
	}
	if err := m.coord.MarkPendingCompletion(ctx, sess.id, turns.TurnID(ts.taskID)); err != nil {
		if errors.Is(err, turns.ErrTurnNotFound) {
			return nil
		}
		return fmt.Errorf("materializer: mark pending %s: %w", ts.taskID, err)
	}
	return nil
}

// preRoute resolves the root turn an event folds into. When this instance
// resumed from a persisted cursor and the event names a task or run it
// never observed spawning, the task's DURABLE ancestry is recovered first
// (the persisted routes: task -> parent, run -> task), so child and
// explicit-run events route exactly as they did before the restart. A
// root row is adopted only through its persisted route or its own stored
// run binding — never by guessing. Events at or below an adopted row's
// last applied sequence are already reflected in it. Returns the resolved
// turn (nil when unroutable).
func (m *Materializer) preRoute(ctx context.Context, sess *sessionState, kind routeKind, key string) (*turnState, error) {
	if key == "" {
		return nil, nil
	}
	if kind == routeRun {
		if taskID, ok := sess.runs[key]; ok {
			return m.resolveTask(ctx, sess, taskID)
		}
		if !m.canAdopt() || sess.adoptMiss["run:"+key] {
			return nil, nil
		}
		r, ok, err := m.coord.LoadRunRoute(ctx, sess.id, key)
		if err != nil {
			return nil, fmt.Errorf("materializer: load run route %s: %w", key, err)
		}
		if ok {
			ts, err := m.ensureKnownTask(ctx, sess, r.TaskID, 0, false)
			if err != nil || ts == nil {
				return ts, err
			}
			if _, bound := sess.runs[key]; !bound {
				if err := sess.validateTaskRoute(r.TaskID, sess.tasks[r.TaskID], key, false); err == nil {
					sess.commitTaskRoute(r.TaskID, sess.tasks[r.TaskID], key, false)
				}
			}
			return ts, nil
		}
		// A root row persisted before routes existed: adopt it only when
		// its OWN stored run binding is exactly this run.
		ts, err := m.adoptRootRow(ctx, sess, key, key)
		if err == nil && ts == nil {
			m.noteAdoptMiss(sess, "run:"+key)
		}
		return ts, err
	}
	return m.resolveTask(ctx, sess, key)
}

// resolveTask walks the task's known ancestry to its root turn,
// recovering an unknown ancestor's durable route when allowed.
func (m *Materializer) resolveTask(ctx context.Context, sess *sessionState, taskID string) (*turnState, error) {
	cur := taskID
	for range 128 {
		parent, ok := sess.tasks[cur]
		if !ok {
			if !m.canAdopt() {
				return nil, nil
			}
			return m.ensureKnownTask(ctx, sess, taskID, 0, false)
		}
		if parent == "" {
			return sess.turns[turns.TurnID(cur)], nil
		}
		cur = parent
	}
	return nil, nil
}

// ensureKnownTask registers taskID and its ancestors in the session's
// routing index from their persisted routes and adopts the root turn's
// durable row. force skips the resumed-from-cursor precondition (the
// pending-queue restore). Returns the root turn (nil when the ancestry or
// the row cannot be proven).
func (m *Materializer) ensureKnownTask(ctx context.Context, sess *sessionState, taskID string, depth int, force bool) (*turnState, error) {
	if depth > 128 || taskID == "" || m.coord == nil || (!force && !m.canAdopt()) {
		return nil, nil
	}
	if _, ok := sess.tasks[taskID]; ok {
		cur := taskID
		for range 128 {
			parent := sess.tasks[cur]
			if parent == "" {
				return sess.turns[turns.TurnID(cur)], nil
			}
			if _, ok := sess.tasks[parent]; !ok {
				return m.ensureKnownTask(ctx, sess, parent, depth+1, force)
			}
			cur = parent
		}
		return nil, nil
	}
	if sess.adoptMiss[taskID] {
		return nil, nil
	}
	r, ok, err := m.coord.LoadTaskRoute(ctx, sess.id, taskID)
	if err != nil {
		return nil, fmt.Errorf("materializer: load task route %s: %w", taskID, err)
	}
	if ok && r.ParentTaskID != "" {
		root, err := m.ensureKnownTask(ctx, sess, r.ParentTaskID, depth+1, force)
		if err != nil {
			return nil, err
		}
		if _, known := sess.tasks[r.ParentTaskID]; !known {
			m.noteAdoptMiss(sess, taskID)
			return nil, nil
		}
		if err := sess.validateTaskRoute(taskID, r.ParentTaskID, r.RunID, false); err != nil {
			m.noteAdoptMiss(sess, taskID)
			return nil, nil
		}
		sess.commitTaskRoute(taskID, r.ParentTaskID, r.RunID, false)
		return root, nil
	}
	wantRun := ""
	if ok {
		wantRun = r.RunID
	}
	ts, err := m.adoptRootRow(ctx, sess, taskID, wantRun)
	if err == nil && ts == nil {
		m.noteAdoptMiss(sess, taskID)
	}
	return ts, err
}

// adoptRootRow adopts the durable row of a root turn this instance never
// observed spawning. The row must exist under its own task id, and when
// wantRun is set its stored run binding must be exactly wantRun. Routing
// is rebuilt from the row's own binding and the accumulators are seeded
// from the row (an overflowed activity window or a dropped reasoning tail
// is frozen at the stored exact totals rather than recomputed from a
// partial feed).
func (m *Materializer) adoptRootRow(ctx context.Context, sess *sessionState, taskID, wantRun string) (*turnState, error) {
	if hasPipe(taskID) {
		return nil, nil
	}
	if ts, ok := sess.turns[turns.TurnID(taskID)]; ok {
		return ts, nil
	}
	row, err := m.proj.Get(ctx, sess.id, turns.TurnID(taskID))
	if err != nil {
		if errors.Is(err, turns.ErrTurnNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("materializer: adopt turn %s: %w", taskID, err)
	}
	if row.TaskID != "" && row.TaskID != taskID {
		return nil, nil
	}
	runID := row.RunID
	if runID == "" {
		runID = taskID // a legacy row without a run binding: the stock derivation
	}
	if wantRun != "" && runID != wantRun {
		return nil, nil
	}
	derived := runID == taskID
	if err := sess.validateTaskRoute(taskID, "", runID, derived); err != nil {
		return nil, nil
	}
	sess.commitTaskRoute(taskID, "", runID, derived)
	ts := &turnState{
		taskID:         taskID,
		runID:          runID,
		sealed:         row.Sealed,
		adoptedThrough: row.LastAppliedEventSeq,
		usage:          row.Usage,
		inputs:         append([]turns.Attachment(nil), row.Inputs...),
	}
	if row.Activity.More {
		ts.activityFrozen = true
	} else {
		ts.activity = append([]turns.ActivityRow(nil), row.Activity.Rows...)
	}
	if row.Reasoning.Dropped > 0 {
		ts.reasoningFrozen = true
	} else {
		ts.reasoning = append([]turns.ReasoningStep(nil), row.Reasoning.Steps...)
		for _, st := range ts.reasoning {
			if st.Index >= ts.nextReasoningIndex {
				ts.nextReasoningIndex = st.Index + 1
			}
		}
	}
	sess.turns[turns.TurnID(taskID)] = ts
	return ts, nil
}

// restorePendingLocked rebuilds the deferred-complete queue of a new
// tenure from the durable pending marks, so a completion whose events are
// behind the resume cursor still converges when its answer appears.
func (m *Materializer) restorePendingLocked(ctx context.Context) error {
	var after turns.IdentityRepairKey
	for {
		keys, err := m.coord.ListPendingCompletions(ctx, after, 256)
		if err != nil {
			return fmt.Errorf("materializer: list pending completions: %w", err)
		}
		for _, k := range keys {
			after = k
			sess, err := m.sessionForLocked(ctx, k.Identity)
			if err != nil {
				return err
			}
			if sess.fenced {
				continue
			}
			ts, err := m.ensureKnownTask(ctx, sess, string(k.TurnID), 0, true)
			if err != nil {
				return err
			}
			if ts != nil && !ts.terminal() {
				m.enqueuePending(sess, ts)
			}
		}
		if len(keys) < 256 {
			return nil
		}
	}
}

func (m *Materializer) noteAdoptMiss(sess *sessionState, taskID string) {
	if sess.adoptMiss == nil || len(sess.adoptMiss) >= maxAdoptMisses {
		sess.adoptMiss = map[string]bool{}
	}
	sess.adoptMiss[taskID] = true
}

// ---------------------------------------------------------------------------
// identity repair
// ---------------------------------------------------------------------------

// repairPassLocked fills, at most repairBudget per pass, the query /
// agent of rows flagged at append with those components unavailable,
// from the authoritative task record (read through the same bound
// snapshot seam as a spawn). A found record settles the row (whatever
// it lacks is genuinely absent); a missing record settles only a
// sealed row older than repairSettleAge. Repair is best effort:
// a failure is logged and the row is retried on a later scan, never a
// pass failure. An exhausted scan idles for repairRescan.
func (m *Materializer) repairPassLocked(ctx context.Context) {
	if m.coord == nil || !m.held || m.snap == nil {
		return
	}
	now := m.now()
	if now.Before(m.repairIdleUntil) {
		return
	}
	keys, err := m.coord.ListIdentityRepairCandidates(ctx, m.repairAfter, m.repairBudget)
	if err != nil {
		m.log(ctx, slog.LevelWarn, "turns identity repair: list candidates failed", slog.Any("error", err))
		return
	}
	if len(keys) == 0 {
		m.repairAfter = turns.IdentityRepairKey{}
		m.repairIdleUntil = now.Add(m.repairRescan)
		return
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		m.repairAfter = k
		err := m.repairOne(m.fencedCtx(ctx), k)
		if m.leaseLostLocked(ctx, err) {
			return
		}
		if err != nil && !errors.Is(err, turns.ErrErasureFenced) && !errors.Is(err, turns.ErrTurnNotFound) {
			m.log(ctx, slog.LevelWarn, "turns identity repair failed",
				slog.String("session_id", k.Identity.SessionID), slog.String("task_id", string(k.TurnID)), slog.Any("error", err))
		}
	}
	if len(keys) < m.repairBudget {
		// The scan reached the end of the flagged rows: idle, then
		// rescan from the start (rows whose record was still unreadable
		// are retried at most once per rescan interval).
		m.repairAfter = turns.IdentityRepairKey{}
		m.repairIdleUntil = now.Add(m.repairRescan)
	}
}

// fillIdentityIfMissing repairs, inline, a row whose query / agent is
// unavailable while the found task snapshot carries it. A no-op without
// a coordination store or when nothing is missing.
func (m *Materializer) fillIdentityIfMissing(ctx context.Context, id identity.Identity, taskID string, row turns.TurnRow, snap TaskSnapshot, found bool) error {
	if m.coord == nil || !found || !turns.NeedsIdentityRepair(row) {
		return nil
	}
	if !(snap.QueryPresent && snap.Query != "" && row.Query.Complete == turns.CompletenessUnavailable) &&
		!(snap.AgentPresent && row.Agent.Complete == turns.CompletenessUnavailable) {
		return nil
	}
	_, changed, err := m.coord.RepairTurnIdentity(ctx, id, turns.TurnID(taskID), turns.IdentityFill{
		QueryPresent: snap.QueryPresent, Query: snap.Query, QueryAt: snap.QueryAt,
		AgentPresent: snap.AgentPresent, AgentID: snap.AgentID, AgentName: snap.AgentName, AgentBindingSource: snap.AgentBindingSource,
		Settle: true,
	})
	if err != nil {
		if errors.Is(err, turns.ErrTurnNotFound) {
			return nil
		}
		return fmt.Errorf("materializer: fill identity %s: %w", taskID, err)
	}
	if changed {
		m.repaired++
	}
	return nil
}

func (m *Materializer) repairOne(ctx context.Context, k turns.IdentityRepairKey) error {
	if sess := m.sessions[k.Identity]; sess != nil && sess.fenced {
		return turns.ErrErasureFenced
	}
	snap, found, err := m.readTaskSnapshotFound(ctx, k.Identity, string(k.TurnID))
	if err != nil {
		return err
	}
	fill := turns.IdentityFill{Settle: found}
	if found {
		fill.QueryPresent, fill.Query, fill.QueryAt = snap.QueryPresent, snap.Query, snap.QueryAt
		fill.AgentPresent, fill.AgentID, fill.AgentName, fill.AgentBindingSource = snap.AgentPresent, snap.AgentID, snap.AgentName, snap.AgentBindingSource
	} else {
		row, err := m.proj.Get(ctx, k.Identity, k.TurnID)
		if err != nil {
			return err
		}
		// The record may still appear (a peer instance has not
		// persisted it where this one reads); only a sealed row whose
		// last write is older than the settle age is given up on.
		if !row.Sealed || m.now().Sub(row.UpdatedAt) < repairSettleAge {
			return nil
		}
		fill.Settle = true
	}
	_, changed, err := m.coord.RepairTurnIdentity(ctx, k.Identity, k.TurnID, fill)
	if err != nil {
		return err
	}
	if changed {
		m.repaired++
	}
	return nil
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func (m *Materializer) log(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if m.logger == nil {
		return
	}
	m.logger.LogAttrs(ctx, level, msg, attrs...)
}

// withSpawnDeferral is the test seam for the live-spawn deferral bounds
// (attempts AND grace must both be exhausted; 0/0 disables deferral).
func withSpawnDeferral(attempts int, grace time.Duration) Option {
	return func(m *Materializer) {
		m.spawnDeferAttempts = attempts
		m.spawnDeferGrace = grace
	}
}
