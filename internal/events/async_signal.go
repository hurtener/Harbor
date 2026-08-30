package events

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// DefaultAsyncAdmissionLogInterval bounds operational warning volume for a
	// saturated or closed asynchronous publication lane. Counters still record
	// every admission failure.
	DefaultAsyncAdmissionLogInterval = time.Second
)

// AsyncAdmissionFailureReason is the low-cardinality classification attached
// to an asynchronous publication admission failure.
type AsyncAdmissionFailureReason string

const (
	// AsyncAdmissionQueueFull means the bounded asynchronous queue had no
	// available slot.
	AsyncAdmissionQueueFull AsyncAdmissionFailureReason = "queue_full"
	// AsyncAdmissionBusClosed means the EventBus had already closed.
	AsyncAdmissionBusClosed AsyncAdmissionFailureReason = "bus_closed"
)

// AsyncAdmissionSignal records and rate-limits operational signals for
// asynchronous publication admission failures. It is safe for concurrent
// use and contains no event payload or identity state. Counters increment for
// every recognized admission failure; only the slog warning is rate-limited.
type AsyncAdmissionSignal struct {
	logger   *slog.Logger
	interval int64

	total     atomic.Int64
	queueFull atomic.Int64
	closed    atomic.Int64
	lastLog   atomic.Int64
}

// NewAsyncAdmissionSignal constructs a per-bus admission signal. A nil logger
// uses slog.Default(). A non-positive interval uses
// DefaultAsyncAdmissionLogInterval.
func NewAsyncAdmissionSignal(logger *slog.Logger, interval time.Duration) *AsyncAdmissionSignal {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultAsyncAdmissionLogInterval
	}
	return &AsyncAdmissionSignal{logger: logger, interval: interval.Nanoseconds()}
}

// Observe records an asynchronous admission failure. Non-admission errors
// are ignored because they are already returned by PublishAsync and do not
// represent bounded-lane loss. The warning is deliberately content-free:
// only a low-cardinality reason and canonical event type are logged. The
// payload is not accepted by this method, so secret-shaped data cannot reach
// the operational signal.
func (s *AsyncAdmissionSignal) Observe(ctx context.Context, eventType EventType, err error) {
	if s == nil {
		return
	}
	reason, ok := asyncAdmissionFailureReason(err)
	if !ok {
		return
	}
	s.total.Add(1)
	switch reason {
	case AsyncAdmissionQueueFull:
		s.queueFull.Add(1)
	case AsyncAdmissionBusClosed:
		s.closed.Add(1)
	}

	now := time.Now().UnixNano()
	last := s.lastLog.Load()
	if last != 0 && now-last < s.interval {
		return
	}
	if !s.lastLog.CompareAndSwap(last, now) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.logger.WarnContext(ctx, "events: asynchronous publication admission failed",
		slog.String("reason", string(reason)),
		slog.String("event_type", string(eventType)),
	)
}

// Total returns the cumulative number of asynchronous publication admission
// failures observed by this signal.
func (s *AsyncAdmissionSignal) Total() int64 {
	if s == nil {
		return 0
	}
	return s.total.Load()
}

// QueueFull returns the cumulative number of bounded-queue saturation
// failures observed by this signal.
func (s *AsyncAdmissionSignal) QueueFull() int64 {
	if s == nil {
		return 0
	}
	return s.queueFull.Load()
}

// Closed returns the cumulative number of after-close admission failures
// observed by this signal.
func (s *AsyncAdmissionSignal) Closed() int64 {
	if s == nil {
		return 0
	}
	return s.closed.Load()
}

func asyncAdmissionFailureReason(err error) (AsyncAdmissionFailureReason, bool) {
	switch {
	case errors.Is(err, ErrAsyncQueueFull):
		return AsyncAdmissionQueueFull, true
	case errors.Is(err, ErrBusClosed):
		return AsyncAdmissionBusClosed, true
	default:
		return "", false
	}
}
