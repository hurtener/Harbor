package serve

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

const (
	// sessionActivityQueueSize bounds the marks waiting for the worker. A
	// full queue drops the new mark: activity marks are best-effort, and the
	// next run start or finish in that session marks it again.
	sessionActivityQueueSize = 256
	// sessionActivityRecentCap bounds the worker's per-session
	// "last marked" memory.
	sessionActivityRecentCap = 4096
	// sessionActivityTimeout bounds the store round trip of one mark.
	sessionActivityTimeout = 2 * time.Second
	// sessionActivityDrainTimeout bounds how long Close waits for queued
	// marks; past it the worker's store calls are cancelled and Close
	// returns without waiting further.
	sessionActivityDrainTimeout = 2 * time.Second
)

// sessionActivityQueue delivers run-activity marks to the session registry
// off the run path. A run only ever does a non-blocking send; one worker
// goroutine applies the marks, skipping a session it marked within the
// coalesce window before it touches the registry (whose writes serialize on
// one record lock held across store I/O). Nil-safe: a nil queue (no marker
// wired) ignores every call.
//
// Concurrency: enqueue is safe from any goroutine between start and stop;
// `recent` is owned by the worker goroutine alone.
type sessionActivityQueue struct {
	marker SessionActivityMarker
	logger *slog.Logger

	coalesce     time.Duration
	drainTimeout time.Duration

	ch     chan identity.Identity
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func newSessionActivityQueue(marker SessionActivityMarker, logger *slog.Logger) *sessionActivityQueue {
	if marker == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionActivityQueue{
		marker:       marker,
		logger:       logger,
		coalesce:     sessions.RunActivityCoalesceWindow,
		drainTimeout: sessionActivityDrainTimeout,
	}
}

// start launches the worker. Called once, from RunLoopDriver.Start.
func (q *sessionActivityQueue) start() {
	if q == nil || q.ch != nil {
		return
	}
	q.ch = make(chan identity.Identity, sessionActivityQueueSize)
	q.done = make(chan struct{})
	q.ctx, q.cancel = context.WithCancel(context.Background())
	go q.loop()
}

// enqueue offers a mark for the session `ident` names without ever
// blocking; a full queue drops it.
func (q *sessionActivityQueue) enqueue(ident identity.Identity) {
	if q == nil || q.ch == nil {
		return
	}
	select {
	case q.ch <- ident:
	default:
		q.logger.Debug("RunLoopDriver: session activity queue full; mark dropped")
	}
}

// stop closes the queue and waits up to drainTimeout for queued marks. On
// timeout (a hung store, or a registry lock held across one) it cancels the
// worker's store calls and returns; the worker exits once its current call
// returns. Must run after every enqueuer has returned.
func (q *sessionActivityQueue) stop() {
	if q == nil || q.ch == nil {
		return
	}
	close(q.ch)
	t := time.NewTimer(q.drainTimeout)
	defer t.Stop()
	select {
	case <-q.done:
	case <-t.C:
		q.logger.Warn("RunLoopDriver: session activity marks not drained before shutdown; abandoning the rest")
	}
	q.cancel()
}

func (q *sessionActivityQueue) loop() {
	defer close(q.done)
	recent := make(map[identity.Identity]time.Time)
	for ident := range q.ch {
		if q.ctx.Err() != nil {
			continue // shutdown gave up: discard the rest without store calls
		}
		now := time.Now()
		if last, ok := recent[ident]; ok && now.Sub(last) < q.coalesce {
			continue
		}
		if len(recent) >= sessionActivityRecentCap {
			for k, at := range recent {
				if now.Sub(at) >= q.coalesce {
					delete(recent, k)
				}
			}
			if len(recent) >= sessionActivityRecentCap {
				clear(recent)
			}
		}
		recent[ident] = now
		q.apply(ident)
	}
}

func (q *sessionActivityQueue) apply(ident identity.Identity) {
	ctx, cancel := context.WithTimeout(q.ctx, sessionActivityTimeout)
	defer cancel()
	if err := q.marker.MarkRunActivity(ctx, ident); err != nil {
		level := slog.LevelWarn
		if errors.Is(err, sessions.ErrSessionNotFound) || errors.Is(err, sessions.ErrRegistryClosed) || errors.Is(err, context.Canceled) {
			// No session record (a runtime without create-on-first-use, or an
			// erased session) or shutdown: nothing to mark.
			level = slog.LevelDebug
		}
		q.logger.Log(ctx, level, "RunLoopDriver: session activity mark failed", slog.String("err", err.Error()))
	}
}
