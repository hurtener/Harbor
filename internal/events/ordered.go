package events

import (
	"context"
	"fmt"
	"sync"
)

const (
	// DefaultAsyncQueueSize is the bounded admission capacity used by the
	// built-in buses when no test/operator override is supplied.
	DefaultAsyncQueueSize = 256
	// DefaultPublishBatchSize bounds opportunistic coalescing and the size of
	// explicit atomic batches accepted through this ingress.
	DefaultPublishBatchSize = 16
)

// BatchCommit is the driver-owned commit/fan-out callback used by
// OrderedQueue. The callback must commit all events or none and must preserve
// their order when notifying projection watermarks and subscribers. The
// generation is the per-identity erasure generation captured when the
// request was admitted; expectedEventID is an optional durable generation
// token that the driver revalidates in its commit transaction.
type BatchCommit func(context.Context, []Event, uint64, string) error

// QueueFailureHandler receives an asynchronous batch error. Synchronous
// callers receive the same error from Publish/PublishBatch/Flush; this hook is
// the only reporting path available to PublishAsync, which acknowledges only
// queue admission.
type QueueFailureHandler func(context.Context, []Event, error)

// OrderedQueue is a bounded FIFO ingress shared by the built-in event buses.
// Ordinary and asynchronous publications enter the same queue, so a later
// synchronous publication cannot overtake earlier telemetry. Adjacent async
// requests for the same identity/session are coalesced; Flush and Close are
// hard barriers.
type OrderedQueue struct {
	commit   BatchCommit
	failure  QueueFailureHandler
	capacity int
	maxBatch int

	mu          sync.Mutex
	queue       []orderedRequest
	changed     chan struct{}
	closed      bool
	started     bool
	terminalErr error
	doneOnce    sync.Once

	workerCtx    context.Context
	workerCancel context.CancelFunc
	done         chan struct{}
}

type orderedRequest struct {
	ctx             context.Context
	events          []Event
	generation      uint64
	expectedEventID string
	wait            chan error
	barrier         bool
	async           bool
}

// NewOrderedQueue constructs a lazy queue. The worker starts on first
// admission, so constructing an otherwise-unused bus does not leak a goroutine.
func NewOrderedQueue(capacity, maxBatch int, commit BatchCommit, failure QueueFailureHandler) (*OrderedQueue, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("events: ordered queue capacity must be > 0")
	}
	if maxBatch <= 0 {
		return nil, fmt.Errorf("events: ordered queue batch size must be > 0")
	}
	if commit == nil {
		return nil, fmt.Errorf("events: ordered queue commit callback is required")
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &OrderedQueue{
		commit:       commit,
		failure:      failure,
		capacity:     capacity,
		maxBatch:     maxBatch,
		changed:      make(chan struct{}),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		done:         make(chan struct{}),
	}, nil
}

// Publish admits one event and waits until its commit outcome is known.
func (q *OrderedQueue) Publish(ctx context.Context, ev Event) error {
	return q.PublishBatch(ctx, []Event{ev})
}

// PublishWithGeneration admits one event with an erasure generation captured
// by the owning event bus and waits until its commit outcome is known.
func (q *OrderedQueue) PublishWithGeneration(ctx context.Context, ev Event, generation uint64) error {
	return q.enqueue(ctx, []Event{ev}, true, false, false, generation, "")
}

// PublishWithGenerationAndExpectation admits one event with an erasure
// generation and an opaque durable generation token captured by the owning
// event bus, then waits until its commit outcome is known.
func (q *OrderedQueue) PublishWithGenerationAndExpectation(ctx context.Context, ev Event, generation uint64, expectedEventID string) error {
	return q.enqueue(ctx, []Event{ev}, true, false, false, generation, expectedEventID)
}

// PublishAsync acknowledges only immediate queue admission. Saturation is a
// visible ErrAsyncQueueFull rather than hidden blocking or silent loss.
func (q *OrderedQueue) PublishAsync(ctx context.Context, ev Event) error {
	return q.enqueue(ctx, []Event{ev}, false, false, true, 0, "")
}

// PublishAsyncWithGeneration acknowledges immediate queue admission for one
// event carrying an erasure generation captured by the owning event bus.
func (q *OrderedQueue) PublishAsyncWithGeneration(ctx context.Context, ev Event, generation uint64) error {
	return q.enqueue(ctx, []Event{ev}, false, false, true, generation, "")
}

// PublishAsyncWithGenerationAndExpectation acknowledges immediate queue
// admission for one event carrying an erasure generation and an opaque
// durable generation token captured by the owning event bus.
func (q *OrderedQueue) PublishAsyncWithGenerationAndExpectation(ctx context.Context, ev Event, generation uint64, expectedEventID string) error {
	return q.enqueue(ctx, []Event{ev}, false, false, true, generation, expectedEventID)
}

// PublishBatch enqueues an explicitly atomic batch and waits for its commit.
// The batch is copied before admission so the caller can safely reuse its
// backing slice after this method returns.
func (q *OrderedQueue) PublishBatch(ctx context.Context, batch []Event) error {
	if len(batch) == 0 {
		return fmt.Errorf("events: ordered publish batch is empty")
	}
	if len(batch) > q.maxBatch {
		return fmt.Errorf("events: ordered publish batch length %d exceeds max %d", len(batch), q.maxBatch)
	}
	return q.enqueue(ctx, append([]Event(nil), batch...), true, false, false, 0, "")
}

// PublishBatchWithGeneration enqueues an explicitly atomic batch with an
// erasure generation captured by the owning event bus and waits for its
// commit. The batch must be homogeneous by identity and generation.
func (q *OrderedQueue) PublishBatchWithGeneration(ctx context.Context, batch []Event, generation uint64) error {
	if len(batch) == 0 {
		return fmt.Errorf("events: ordered publish batch is empty")
	}
	if len(batch) > q.maxBatch {
		return fmt.Errorf("events: ordered publish batch length %d exceeds max %d", len(batch), q.maxBatch)
	}
	return q.enqueue(ctx, append([]Event(nil), batch...), true, false, false, generation, "")
}

// PublishBatchWithGenerationAndExpectation enqueues an explicitly atomic
// batch with an erasure generation and an opaque durable generation token
// captured by the owning event bus, then waits for its commit. The batch must
// be homogeneous by identity and generation.
func (q *OrderedQueue) PublishBatchWithGenerationAndExpectation(ctx context.Context, batch []Event, generation uint64, expectedEventID string) error {
	if len(batch) == 0 {
		return fmt.Errorf("events: ordered publish batch is empty")
	}
	if len(batch) > q.maxBatch {
		return fmt.Errorf("events: ordered publish batch length %d exceeds max %d", len(batch), q.maxBatch)
	}
	return q.enqueue(ctx, append([]Event(nil), batch...), true, false, false, generation, expectedEventID)
}

// Flush places a barrier after every earlier accepted request. It returns the
// first commit failure since the preceding barrier.
func (q *OrderedQueue) Flush(ctx context.Context) error {
	return q.enqueue(ctx, nil, true, true, false, 0, "")
}

// Close atomically rejects new admissions and waits for all accepted requests.
// If ctx expires, the worker is cancelled and still joined before Close
// returns, so a driver can never close its StateStore beneath an in-flight
// commit. Repeated calls return the same terminal queue error.
func (q *OrderedQueue) Close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("events: ordered queue close context is nil")
	}

	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.signalLocked()
		if !q.started {
			q.finishLocked(nil)
		}
	}
	done := q.done
	q.mu.Unlock()

	select {
	case <-done:
		return q.result()
	case <-ctx.Done():
		q.workerCancel()
		<-done
		return ctx.Err()
	}
}

func (q *OrderedQueue) enqueue(ctx context.Context, payload []Event, wait, barrier, async bool, generation uint64, expectedEventID string) error {
	if ctx == nil {
		return fmt.Errorf("events: ordered queue context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request := orderedRequest{ctx: ctx, events: payload, generation: generation, expectedEventID: expectedEventID, barrier: barrier, async: async}
	if wait {
		request.wait = make(chan error, 1)
	}

	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return ErrBusClosed
		}
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			return err
		}
		if len(q.queue) < q.capacity {
			q.startLocked()
			q.queue = append(q.queue, request)
			q.signalLocked()
			q.mu.Unlock()
			break
		}
		if async {
			q.mu.Unlock()
			return ErrAsyncQueueFull
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if !wait {
		return nil
	}
	// Once admitted, wait for the definitive commit result. Returning merely
	// because ctx was cancelled could report failure after a successful commit;
	// the worker receives that cancellation and resolves the outcome instead.
	return <-request.wait
}

func (q *OrderedQueue) startLocked() {
	if q.started {
		return
	}
	q.started = true
	go q.run()
}

func (q *OrderedQueue) signalLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func (q *OrderedQueue) finishLocked(err error) {
	q.terminalErr = err
	q.doneOnce.Do(func() { close(q.done) })
}

func (q *OrderedQueue) result() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.terminalErr
}

func (q *OrderedQueue) run() {
	var pendingErr error
	for {
		requests, batch, barrier, stop, drained := q.nextWork(pendingErr)
		if stop {
			for _, request := range drained {
				q.completeDrained(request, q.workerCtx.Err())
			}
			return
		}
		if barrier != nil {
			completeRequest(*barrier, pendingErr)
			pendingErr = nil
			continue
		}

		workCtx, cancel := q.workContext(requests[0])
		err := q.commit(workCtx, batch, requests[0].generation, requests[0].expectedEventID)
		if err != nil && requests[0].async && q.failure != nil {
			q.failure(workCtx, batch, err)
		}
		cancel()
		if err != nil && requests[0].async && pendingErr == nil {
			pendingErr = err
		}
		for _, request := range requests {
			completeRequest(request, err)
		}
	}
}

// nextWork removes one request, opportunistically coalescing only async
// events. Synchronous requests retain independent contexts and commit results.
func (q *OrderedQueue) nextWork(pendingErr error) (requests []orderedRequest, batch []Event, barrier *orderedRequest, stop bool, drained []orderedRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.closed && q.workerCtx.Err() == nil {
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-q.workerCtx.Done():
		}
		q.mu.Lock()
	}
	if q.workerCtx.Err() != nil {
		drained = append(drained, q.queue...)
		q.queue = nil
		terminalErr := pendingErr
		if terminalErr == nil {
			terminalErr = q.workerCtx.Err()
		}
		q.finishLocked(terminalErr)
		q.signalLocked()
		return nil, nil, nil, true, drained
	}
	if len(q.queue) == 0 && q.closed {
		q.finishLocked(pendingErr)
		return nil, nil, nil, true, nil
	}

	first := q.queue[0]
	q.queue = q.queue[1:]
	if first.barrier {
		q.signalLocked()
		return nil, nil, &first, false, nil
	}
	requests = append(requests, first)
	batch = append(batch, first.events...)
	if first.async {
		for len(q.queue) > 0 && len(batch) < q.maxBatch {
			next := q.queue[0]
			if next.barrier || !compatibleRequest(first, next) || len(batch)+len(next.events) > q.maxBatch {
				break
			}
			q.queue = q.queue[1:]
			requests = append(requests, next)
			batch = append(batch, next.events...)
		}
	}
	q.signalLocked()
	return requests, batch, nil, false, nil
}

func (q *OrderedQueue) workContext(request orderedRequest) (context.Context, func()) {
	if request.async {
		return requestValuesContext{
			Context: q.workerCtx,
			values:  context.WithoutCancel(request.ctx),
		}, func() {}
	}
	workCtx, cancel := context.WithCancel(request.ctx)
	stop := context.AfterFunc(q.workerCtx, cancel)
	return workCtx, func() {
		stop()
		cancel()
	}
}

func completeRequest(request orderedRequest, err error) {
	if request.wait != nil {
		request.wait <- err
	}
}

type requestValuesContext struct {
	context.Context
	values context.Context
}

func (c requestValuesContext) Value(key any) any { return c.values.Value(key) }

func compatibleRequest(left, right orderedRequest) bool {
	if !left.async || !right.async || len(left.events) == 0 || len(right.events) == 0 {
		return false
	}
	return left.generation == right.generation &&
		left.expectedEventID == right.expectedEventID &&
		sameSession(left.events) && sameSession(right.events) &&
		sameSessionIdentity(left.events[0], right.events[0])
}

func sameSession(batch []Event) bool {
	for i := 1; i < len(batch); i++ {
		if !sameSessionIdentity(batch[0], batch[i]) {
			return false
		}
	}
	return true
}

func sameSessionIdentity(left, right Event) bool {
	return left.Identity.TenantID == right.Identity.TenantID &&
		left.Identity.UserID == right.Identity.UserID &&
		left.Identity.SessionID == right.Identity.SessionID
}

func (q *OrderedQueue) completeDrained(request orderedRequest, err error) {
	if request.async && q.failure != nil {
		q.failure(request.ctx, request.events, err)
	}
	completeRequest(request, err)
}
