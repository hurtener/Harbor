package durable

// The bus owns one bounded maintenance goroutine in durable mode. It runs the
// always-on v1→v2 index migrator (D-492) and, when
// events.retention.audit_max_age > 0, the audit-retention sweeper (D-493).
// Neither ever takes publishMu: the header CAS serializes them against
// publishers in this and every other process. Close cancels the goroutine
// and waits for it before the store is released.

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

const (
	// maintenanceInitialDelay keeps boot and the first requests free of
	// maintenance I/O.
	maintenanceInitialDelay = 2 * time.Minute
	// migrationInterval is the cadence of the background migrator.
	migrationInterval = time.Hour
	// migrationNudgeMinGap debounces publish-path nudges.
	migrationNudgeMinGap = time.Minute
	// retentionInterval is the sweeper cadence (±10 % jitter).
	retentionInterval = 15 * time.Minute
	// retentionDeleteBudget bounds storage deletes per sweeper pass.
	retentionDeleteBudget = 2000
	// retentionDeletesPerSecond paces deletes within a pass.
	retentionDeletesPerSecond = 20
	// retentionSessionDeleteSlice bounds one session's deletes per pass (two
	// chunks' worth), so one large backlog cannot starve other sessions.
	retentionSessionDeleteSlice = 2*indexChunkRows + 2
	// retentionMaxChunksPerSession bounds chunk loads per session per pass.
	retentionMaxChunksPerSession = 64
	// retentionHeadsWarn is the traversal size above which a pass logs the
	// cost of its complete head scan.
	retentionHeadsWarn = 200_000
	// orphanChunkMinAge is how old a mismatching pre-written chunk on a
	// still-legacy session must be before the migrator replaces it.
	orphanChunkMinAge = 24 * time.Hour
	// migrationBackoffBase / migrationBackoffMax bound per-session retries.
	migrationBackoffBase = time.Hour
	migrationBackoffMax  = 24 * time.Hour
	// migrationPrewriteBatch is the number of chunks per pre-write batch.
	migrationPrewriteBatch = 16
	// migrationFlipAttempts bounds header-flip CAS retries per pass.
	migrationFlipAttempts = 8
	// migrationPrewritePause / migrationSessionPause pace the migrator
	// between pre-write batches and after each large session.
	migrationPrewritePause = 50 * time.Millisecond
	migrationSessionPause  = time.Second
	// largeMigrationBytes is the legacy head size after which the migrator
	// pauses before the next session.
	largeMigrationBytes = 256 << 10
)

// maintenance is the bus's background index maintenance state.
type maintenance struct {
	auditMaxAge       time.Duration
	initialDelay      time.Duration
	migrateInterval   time.Duration
	retentionInterval time.Duration
	deleteBudget      int
	deleteSlice       int
	deleteInterval    time.Duration
	// prewritePause / sessionPause pace the migrator between pre-write
	// batches and between large sessions.
	prewritePause time.Duration
	sessionPause  time.Duration
	disabled      bool

	nudge  chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	// passMu serializes passes (the loop and direct test invocations) and
	// guards the per-pass state below.
	passMu    sync.Mutex
	backoff   map[string]migrationBackoff
	resumeKey string
	// stuck mirrors the sessions currently in migration backoff so the
	// publish path can skip nudging for them without taking passMu.
	stuck sync.Map
}

type migrationBackoff struct {
	next  time.Time
	delay time.Duration
}

func (m *maintenance) init(auditMaxAge time.Duration) {
	m.auditMaxAge = auditMaxAge
	m.initialDelay = maintenanceInitialDelay
	m.migrateInterval = migrationInterval
	m.retentionInterval = retentionInterval
	m.deleteBudget = retentionDeleteBudget
	m.deleteSlice = retentionSessionDeleteSlice
	m.deleteInterval = time.Second / retentionDeletesPerSecond
	m.prewritePause = migrationPrewritePause
	m.sessionPause = migrationSessionPause
	m.nudge = make(chan struct{}, 1)
	m.backoff = map[string]migrationBackoff{}
}

// withMaintenance adjusts maintenance timing/budgets. Test seam.
func withMaintenance(fn func(*maintenance)) Option {
	return func(b *bus) { fn(&b.maint) }
}

func (b *bus) startMaintenance() {
	if b.store == nil || b.maint.disabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.maint.cancel = cancel
	b.maint.done = make(chan struct{})
	go b.maintenanceLoop(ctx)
}

func (b *bus) stopMaintenance() {
	if b.maint.cancel == nil {
		return
	}
	b.maint.cancel()
	<-b.maint.done
}

// nudgeMigrator asks the background migrator for an early pass. Never blocks.
// A session already in migration backoff is not nudged: its next attempt is
// scheduled, and repeated nudges would only repeat the full head scan.
func (b *bus) nudgeMigrator(id identity.Quadruple) {
	if _, stuck := b.maint.stuck.Load(sessionMapKey(id)); stuck {
		return
	}
	select {
	case b.maint.nudge <- struct{}{}:
	default:
	}
}

func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 5 // ±10 %
	if spread <= 0 {
		return d
	}
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1)) //nolint:gosec // scheduling jitter, not a security value
}

func (b *bus) maintenanceLoop(ctx context.Context) {
	defer close(b.maint.done)
	m := &b.maint
	start := time.Now()
	nextMigrate := start.Add(m.initialDelay)
	var lastMigrate time.Time
	retention := m.auditMaxAge > 0
	nextRetention := start.Add(m.initialDelay + jittered(m.retentionInterval)/4)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wake := nextMigrate
		if retention && nextRetention.Before(wake) {
			wake = nextRetention
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Until(wake))
		select {
		case <-ctx.Done():
			return
		case <-m.nudge:
			// A nudge can bring a pass forward, but never ahead of the boot
			// quiet period or within the debounce gap of the last pass.
			earliest := lastMigrate.Add(migrationNudgeMinGap)
			if quiet := start.Add(m.initialDelay); earliest.Before(quiet) {
				earliest = quiet
			}
			if earliest.Before(time.Now()) {
				earliest = time.Now()
			}
			if earliest.Before(nextMigrate) {
				nextMigrate = earliest
			}
			continue
		case <-timer.C:
		}
		now := time.Now()
		if !now.Before(nextMigrate) {
			if _, err := b.runMigrationPass(ctx); err != nil && ctx.Err() == nil {
				b.logger.WarnContext(ctx, "durable: index migration pass failed", slog.String("driver", "durable"), slog.String("error", err.Error()))
			}
			lastMigrate = time.Now()
			nextMigrate = lastMigrate.Add(m.migrateInterval)
		}
		if retention && !now.Before(nextRetention) {
			if _, err := b.runRetentionPass(ctx); err != nil && ctx.Err() == nil {
				b.logger.WarnContext(ctx, "durable: audit retention pass failed", slog.String("driver", "durable"), slog.String("error", err.Error()))
			}
			nextRetention = time.Now().Add(jittered(m.retentionInterval))
		}
		if ctx.Err() != nil {
			return
		}
	}
}
